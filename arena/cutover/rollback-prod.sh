#!/usr/bin/env bash
# Undo cutover-prod.sh: the binary, server.toml and unit from the backups it
# took, Caddy started again, 8741 closed again where the cutover opened it,
# then MCP checked through Caddy. Run from the Mac. Converges when rerun.
#
# Usage: arena/cutover/rollback-prod.sh SSH_HOST PUBLIC_HOST
# Env:
#   HCLOUD_FIREWALL  the firewall id given to cutover-prod.sh, if any
#   CADDY            Caddy container name, default loyal-agent-memory-caddy
#   TOKEN_FILE       read token for the MCP check, default ~/.mnemonic/token
#   CA_FILE          extra CA bundle for the MCP check (Caddy on Let's Encrypt staging)
#   FORCE_TOML=1     restore server.toml even if it changed beyond the
#                    cutover's lines since the backup (tokens added later are lost)
set -euo pipefail

if [[ $# -ne 2 ]]; then
	sed -n '2,/^set -euo/p' "$0" >&2
	exit 2
fi
H=$1 PUBLIC=$2
CADDY=${CADDY:-loyal-agent-memory-caddy}
TOKEN_FILE=${TOKEN_FILE:-$HOME/.mnemonic/token}
HCLOUD_FIREWALL=${HCLOUD_FIREWALL:-}
B=/root/mnemonic-precutover

ok() { printf '%s ok   %s\n' "$(date +%H:%M:%S)" "$*"; }
fail() {
	printf '%s FAIL %s\n' "$(date +%H:%M:%S)" "$*" >&2
	exit 1
}
remote() { ssh -o ConnectTimeout=10 -o BatchMode=yes "$H" "$@"; }

t0=$(date +%s)
since=$(remote 'date -u "+%F %T"')
remote bash -s -- "$B" "$CADDY" "${FORCE_TOML:-}" <<-'EOF'
	set -euo pipefail
	B=$1 CADDY=$2 FORCE=${3:-}
	[[ -f $B/DONE ]] || { echo "FAIL r1: no backups in $B" >&2; exit 1; }
	(cd "$B" && sha256sum -c --quiet SHA256SUMS) || { echo "FAIL r1: backup sums do not match" >&2; exit 1; }
	strip() { grep -vE '^(listen|push_listen|hosts|acme_directory)[[:space:]]*=|^\[tls\]|^$' "$1"; }
	if ! cmp -s <(strip /etc/mnemonic/server.toml) <(strip "$B/server.toml") && [[ $FORCE != 1 ]]; then
		echo "FAIL r1: /etc/mnemonic/server.toml changed beyond listen/push_listen/[tls] since the backup (a token added?); merge by hand or FORCE_TOML=1" >&2
		exit 1
	fi
	echo "ok   r1: backups from $(cat "$B/DONE") verified"

	systemctl stop mnemonic
	install -o root -g root -m 755 "$B/mnemonic.bin" /usr/local/bin/mnemonic
	install -o root -g root -m 644 "$B/mnemonic.service" /etc/systemd/system/mnemonic.service
	install -o root -g mnemonic -m 640 "$B/server.toml" /etc/mnemonic/server.toml
	systemctl daemon-reload
	cmp -s "$B/mnemonic.bin" /usr/local/bin/mnemonic && cmp -s "$B/server.toml" /etc/mnemonic/server.toml && cmp -s "$B/mnemonic.service" /etc/systemd/system/mnemonic.service ||
		{ echo "FAIL r2: restored files differ from backups" >&2; exit 1; }
	echo "ok   r2: mnemonic stopped; binary $(sha256sum /usr/local/bin/mnemonic | cut -c1-12), server.toml and unit restored"

	if [[ $(cat "$B/caddy-running") == true && $(docker inspect -f '{{.State.Running}}' "$CADDY") != true ]]; then
		docker start "$CADDY" >/dev/null
	fi
	[[ $(cat "$B/caddy-running") != true || $(docker inspect -f '{{.State.Running}}' "$CADDY") == true ]] || { echo "FAIL r3: $CADDY not running" >&2; exit 1; }
	echo "ok   r3: $CADDY running=$(docker inspect -f '{{.State.Running}}' "$CADDY")"

	if [[ -f $B/ufw-added-8741 ]]; then
		ufw delete allow 8741/tcp >/dev/null
		rm -f "$B/ufw-added-8741"
	fi
	echo "ok   r4: ufw $(ufw status 2>/dev/null | head -1)"

	systemctl start mnemonic
EOF

if [[ -n "$HCLOUD_FIREWALL" ]] && remote "test -f $B/hcloud-added-8741"; then
	hcloud firewall delete-rule "$HCLOUD_FIREWALL" --direction in --protocol tcp --port 8741 \
		--source-ips 0.0.0.0/0 --source-ips ::/0 --description mnemonic-push >/dev/null
	remote "rm -f $B/hcloud-added-8741"
fi
if [[ -n "$HCLOUD_FIREWALL" ]]; then
	if hcloud firewall describe "$HCLOUD_FIREWALL" -o json | jq -e '.rules[] | select(.direction=="in" and .port=="8741")' >/dev/null; then
		fail "r4: hcloud firewall $HCLOUD_FIREWALL still has an inbound 8741 rule"
	fi
	ok "r4: hcloud firewall $HCLOUD_FIREWALL has no 8741 rule"
fi

for ((i = 0; i < 300; i += 3)); do
	remote "journalctl -u mnemonic --since '$since' -o cat | grep -q 'mnemonic serve: HTTP'" && break
	[[ $(remote 'systemctl show mnemonic -p ActiveState --value') == failed ]] && fail "r5: unit failed"
	sleep 3
done
ok "r5: $(remote "journalctl -u mnemonic --since '$since' -o cat | grep 'mnemonic serve' | tail -1")"

TMP=$(mktemp -d)
chmod 700 "$TMP"
trap 'rm -f "$TMP/auth" "$TMP/out"; rmdir "$TMP"' EXIT
printf 'Authorization: Bearer %s\n' "$(tr -d '\n' <"$TOKEN_FILE")" >"$TMP/auth"
ca=()
[[ -n "${CA_FILE:-}" ]] && ca=(--cacert "$CA_FILE")
# list_sessions needs a window with explicit timezones; the last day is enough to prove the index answers.
body=$(printf '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{"limit":1,"start_datetime":"%s","end_datetime":"%s"}}}' \
	"$(date -u -v-1d +%FT%TZ)" "$(date -u +%FT%TZ)")
code=000 iserr=unset
for ((i = 0; i < 300; i += 3)); do
	code=$(curl -sS ${ca[@]+"${ca[@]}"} -o "$TMP/out" -w '%{http_code} %{http_version}' --max-time 60 \
		-H @"$TMP/auth" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-06-18' \
		--data "$body" "https://$PUBLIC/mcp" 2>/dev/null || true)
	iserr=$(jq -r '.result.isError // false' "$TMP/out" 2>/dev/null || echo unparsed)
	[[ "$code" == 200* && "$iserr" == false ]] && break
	sleep 3
done
rm -f "$TMP/out"
[[ "$code" == 200* && "$iserr" == false ]] || fail "r6: MCP list_sessions through Caddy answered $code isError=$iserr"
ok "r6: MCP list_sessions through Caddy: HTTP $code, isError=false; issuer $(openssl s_client -connect "$PUBLIC:443" -servername "$PUBLIC" </dev/null 2>/dev/null | openssl x509 -noout -issuer 2>/dev/null)"
ok "rollback done in $(($(date +%s) - t0)) s; Mac client: arena/cutover/mac-client.sh rollback"
