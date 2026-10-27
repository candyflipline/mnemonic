#!/usr/bin/env bash
# One-shot move of a Caddy-fronted mnemonic server (prod at 19c8a0e) to the
# self-TLS shape in ops/: mnemonic serves 443 (MCP, HTTP) and 8741 (push) with
# one autocert certificate. Run from the Mac in a checkout of the commit that
# ships; ops/ comes from that checkout. Each step checks its result and the
# script stops at the first failure. Every step converges when rerun.
#
# Usage: arena/cutover/cutover-prod.sh SSH_HOST PUBLIC_HOST SERVER_BIN
#   SSH_HOST     root@5.78.147.233
#   PUBLIC_HOST  loyal-agent-memory.5-78-147-233.sslip.io
#   SERVER_BIN   linux/amd64 server build of this checkout (-tags server)
# Env:
#   STEPS            steps to run, default all; they run in the order a b c d f e g
#                    (8741 opens before Caddy stops, so the firewall call is not downtime)
#   DATA_DIR         the server's data_dir, default /var/lib/mnemonic
#   HEAVY_MAX_BYTES  step a (arena/box/upl/hot-shape.sh, read only) stops the run when
#   LIGHT_MAX_BYTES  the hot set's heavy or light bytes are above these (defaults
#                    1570000000 and 64000000, the boot-memory gate P4B2G set) or any
#                    hot part is missing
#   ACME_DIRECTORY   empty for Let's Encrypt production; the staging URL on a rehearsal
#   HCLOUD_FIREWALL  hcloud firewall id holding the host's inbound rules; empty: none
#   CADDY            Caddy container name, default loyal-agent-memory-caddy
#   TOKEN_FILE       read token for the MCP check, default ~/.mnemonic/token
set -euo pipefail

if [[ $# -ne 3 ]]; then
	sed -n '2,/^set -euo/p' "$0" >&2
	exit 2
fi
H=$1 PUBLIC=$2 BIN=$3
STEPS=${STEPS:-a b c d e f g}
CADDY=${CADDY:-loyal-agent-memory-caddy}
DATA_DIR=${DATA_DIR:-/var/lib/mnemonic}
HEAVY_MAX_BYTES=${HEAVY_MAX_BYTES:-1570000000}
LIGHT_MAX_BYTES=${LIGHT_MAX_BYTES:-64000000}
TOKEN_FILE=${TOKEN_FILE:-$HOME/.mnemonic/token}
ACME_DIRECTORY=${ACME_DIRECTORY:-}
HCLOUD_FIREWALL=${HCLOUD_FIREWALL:-}
B=/root/mnemonic-precutover
SRC=/root/mnemonic-cutover-src
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

ok() { printf '%s ok   %s\n' "$(date +%H:%M:%S)" "$*"; }
fail() {
	printf '%s FAIL %s\n' "$(date +%H:%M:%S)" "$*" >&2
	exit 1
}
remote() { ssh -o ConnectTimeout=10 -o BatchMode=yes "$H" "$@"; }
want() { [[ " $STEPS " == *" $1 "* ]]; }

step_a() {
	local out heavy light missing
	out=$(remote sh -s "$DATA_DIR" <"$REPO/arena/box/upl/hot-shape.sh") || fail "a: hot-shape.sh failed on $H"
	field() { sed -n "s/.* $1 \([0-9][0-9]*\).*/\1/p" <<<" $out"; }
	heavy=$(field heavy_bytes) light=$(field light_bytes) missing=$(field missing)
	[[ -n "$heavy" && -n "$light" && -n "$missing" ]] || fail "a: hot-shape.sh printed no heavy_bytes/light_bytes/missing: $out"
	((heavy <= HEAVY_MAX_BYTES && light <= LIGHT_MAX_BYTES && missing == 0)) ||
		fail "a: NO GO, nothing changed: heavy $heavy (max $HEAVY_MAX_BYTES), light $light (max $LIGHT_MAX_BYTES), missing $missing (max 0)"
	ok "a: GO: $out"
}

step_b() {
	remote bash -s -- "$B" "$CADDY" <<-'EOF'
		set -euo pipefail
		B=$1 CADDY=$2
		if [[ -f $B/DONE ]]; then
			cd "$B" && sha256sum -c --quiet SHA256SUMS
			echo "ok   b: backups from $(cat DONE) kept, sums match"
			exit 0
		fi
		mkdir -p -m 700 "$B"
		cp -p /usr/local/bin/mnemonic "$B/mnemonic.bin"
		cp -p /etc/mnemonic/server.toml "$B/server.toml"
		cp -p /etc/systemd/system/mnemonic.service "$B/mnemonic.service"
		running=$(docker inspect -f '{{.State.Running}}' "$CADDY")
		dir=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$CADDY")
		echo "$running" >"$B/caddy-running"
		echo "$dir" >"$B/caddy-dir"
		files=(mnemonic.bin server.toml mnemonic.service)
		for f in Caddyfile docker-compose.yml; do
			if [[ -f $dir/$f ]]; then cp -p "$dir/$f" "$B/$f"; files+=("$f"); fi
		done
		(cd "$B" && sha256sum "${files[@]}" >SHA256SUMS && sha256sum -c --quiet SHA256SUMS)
		date -u +%FT%TZ >"$B/DONE"
		echo "ok   b: backed up ${files[*]} to $B (caddy running=$running)"
	EOF
}

step_c() {
	local sha rsha
	sha=$(shasum -a 256 "$BIN" | cut -d' ' -f1)
	remote "mkdir -p $SRC/systemd"
	scp -q "$BIN" "$H:$SRC/mnemonic.bin"
	scp -q "$REPO/ops/install-server.sh" "$H:$SRC/install-server.sh"
	scp -q "$REPO/ops/systemd/mnemonic.service" "$H:$SRC/systemd/mnemonic.service"
	rsha=$(remote "sha256sum $SRC/mnemonic.bin" | cut -d' ' -f1)
	[[ "$rsha" == "$sha" ]] || fail "c: copied binary sha $rsha != local $sha"
	remote bash -s -- "$SRC" "$sha" <<-'EOF'
		set -euo pipefail
		SRC=$1 SHA=$2
		chmod 755 "$SRC/mnemonic.bin"
		bash "$SRC/install-server.sh" "$SRC/mnemonic.bin" >/dev/null
		got=$(sha256sum /usr/local/bin/mnemonic | cut -d' ' -f1)
		[[ $got == "$SHA" ]] || { echo "FAIL c: installed sha $got != $SHA" >&2; exit 1; }
		cmp -s "$SRC/systemd/mnemonic.service" /etc/systemd/system/mnemonic.service || { echo "FAIL c: unit differs from ops/" >&2; exit 1; }
		[[ $(systemctl show mnemonic -p NeedDaemonReload --value) == no ]] || { echo "FAIL c: unit needs daemon-reload" >&2; exit 1; }
		grep -q '^AmbientCapabilities=CAP_NET_BIND_SERVICE' /etc/systemd/system/mnemonic.service || { echo "FAIL c: unit lacks CAP_NET_BIND_SERVICE" >&2; exit 1; }
		echo "ok   c: /usr/local/bin/mnemonic sha256 $SHA, unit = ops/ (running process unchanged until step g)"
	EOF
}

step_d() {
	remote bash -s -- "$PUBLIC" "$ACME_DIRECTORY" <<-'EOF'
		set -euo pipefail
		PUBLIC=$1 ACME=$2
		f=/etc/mnemonic/server.toml
		tmp=$(mktemp)
		trap 'rm -f "$tmp"' EXIT
		sed -E 's/^listen[[:space:]]*=.*/listen   = ":443"/; s/^push_listen[[:space:]]*=.*/push_listen = ":8741"/' "$f" >"$tmp"
		want=$(printf '[tls]\nhosts = ["%s"]\n' "$PUBLIC")
		[[ -n $ACME ]] && want+=$(printf '\nacme_directory = "%s"' "$ACME")
		if grep -q '^\[tls\]' "$tmp"; then
			have=$(sed -n '/^\[tls\]/,/^\[/p' "$tmp" | grep -E '^(\[tls\]|hosts|acme_directory)')
			[[ $have == "$want" ]] || { echo "FAIL d: $f has a [tls] block that is not hosts=[$PUBLIC] acme=${ACME:-prod}; fix by hand" >&2; exit 1; }
		else
			printf '\n%s\n' "$want" >>"$tmp"
		fi
		[[ $(grep -c '^listen   = ":443"$' "$tmp") == 1 && $(grep -c '^push_listen = ":8741"$' "$tmp") == 1 ]] || { echo "FAIL d: listen/push_listen not exactly once" >&2; exit 1; }
		if cmp -s "$tmp" "$f"; then
			echo "ok   d: $f already has listen :443, push_listen :8741, [tls] $PUBLIC"
			exit 0
		fi
		install -o root -g mnemonic -m 640 "$tmp" "$f"
		echo "ok   d: $f now listen :443, push_listen :8741, [tls] hosts [$PUBLIC] acme ${ACME:-letsencrypt-prod}; tokens untouched ($(grep -c '^\[\[tokens\]\]' "$f") blocks)"
	EOF
}

step_e() {
	remote bash -s -- "$CADDY" <<-'EOF'
		set -euo pipefail
		CADDY=$1
		if [[ $(docker inspect -f '{{.State.Running}}' "$CADDY") == true ]]; then
			docker stop -t 10 "$CADDY" >/dev/null
		fi
		[[ $(docker inspect -f '{{.State.Running}}' "$CADDY") == false ]] || { echo "FAIL e: $CADDY still running" >&2; exit 1; }
		if ss -Htlnp 'sport = :443' | grep -v '(("mnemonic",' | grep -q .; then echo "FAIL e: something other than mnemonic listens on 443" >&2; ss -Htlnp 'sport = :443' >&2; exit 1; fi
		echo "ok   e: $CADDY stopped (container kept, restart=$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$CADDY")), 443 free or mnemonic's"
	EOF
}

step_f() {
	remote bash -s -- "$B" <<-'EOF'
		set -euo pipefail
		B=$1
		if ufw status 2>/dev/null | grep -q '^Status: active'; then
			if ! ufw status | grep -qE '^8741/tcp +ALLOW'; then
				ufw allow 8741/tcp >/dev/null && touch "$B/ufw-added-8741"
			fi
			ufw status | grep -qE '^8741/tcp +ALLOW' || { echo "FAIL f: ufw has no 8741 rule" >&2; exit 1; }
			echo "ok   f: ufw allows 8741/tcp"
		else
			echo "ok   f: ufw inactive, nothing to open"
		fi
	EOF
	if [[ -n "$HCLOUD_FIREWALL" ]]; then
		if ! hcloud firewall describe "$HCLOUD_FIREWALL" -o json | jq -e '.rules[] | select(.direction=="in" and .port=="8741")' >/dev/null; then
			hcloud firewall add-rule "$HCLOUD_FIREWALL" --direction in --protocol tcp --port 8741 \
				--source-ips 0.0.0.0/0 --source-ips ::/0 --description mnemonic-push >/dev/null
			remote "touch $B/hcloud-added-8741"
		fi
		hcloud firewall describe "$HCLOUD_FIREWALL" -o json | jq -e '.rules[] | select(.direction=="in" and .port=="8741")' >/dev/null ||
			fail "f: hcloud firewall $HCLOUD_FIREWALL has no inbound 8741 rule"
		ok "f: hcloud firewall $HCLOUD_FIREWALL allows 8741 inbound"
	fi
	local code
	code=$(nc -z -w 5 "$PUBLIC" 8741 >/dev/null 2>&1 && echo open || echo closed)
	ok "f: $PUBLIC:8741 reachable from here: $code (before step g this is the old listener, if any)"
}

issuer_of() {
	openssl s_client -connect "$PUBLIC:$1" -servername "$PUBLIC" </dev/null 2>/dev/null |
		openssl x509 -noout -issuer -enddate 2>/dev/null | tr '\n' ' '
}

step_g() {
	local since t0 want_issuer cafile hdr code got port i
	TMP=$(mktemp -d)
	chmod 700 "$TMP"
	trap 'rm -f "$TMP/ca.pem" "$TMP/auth"; rmdir "$TMP"' EXIT
	t0=$(date +%s)
	since=$(remote 'date -u "+%F %T"')
	remote 'systemctl restart mnemonic'
	for ((i = 0; i < 300; i += 3)); do
		if remote "journalctl -u mnemonic --since '$since' -o cat | grep -q 'push serving'"; then break; fi
		[[ $(remote 'systemctl show mnemonic -p ActiveState --value') == failed ]] && fail "g: unit failed: $(remote "journalctl -u mnemonic --since '$since' -o cat | tail -5")"
		sleep 3
	done
	remote "journalctl -u mnemonic --since '$since' -o cat | grep -q 'push serving'" || fail "g: no 'push serving' within 300 s"
	ok "g: restarted; push serving after $(($(date +%s) - t0)) s: $(remote "journalctl -u mnemonic --since '$since' -o cat | grep 'mnemonic serve: HTTP' | tail -1")"

	if [[ -n "$ACME_DIRECTORY" ]]; then want_issuer="Let's Encrypt.*STAGING"; else want_issuer="Let's Encrypt, CN ?= ?[^(]|Let's Encrypt/CN=[^(]"; fi
	for port in 443 8741; do
		got=''
		for ((i = 0; i < 20; i++)); do
			got=$(issuer_of "$port" || true)
			[[ "$got" =~ $want_issuer ]] && break
			sleep 3
		done
		[[ "$got" =~ $want_issuer ]] || fail "g: $PUBLIC:$port certificate issuer is not $want_issuer: ${got:-no TLS}"
		ok "g: $PUBLIC:$port $got"
	done

	cafile=$TMP/ca.pem
	hdr=$TMP/auth
	printf 'Authorization: Bearer %s\n' "$(tr -d '\n' <"$TOKEN_FILE")" >"$hdr"
	local curlca=()
	if [[ -n "$ACME_DIRECTORY" ]]; then
		curl -fsS https://letsencrypt.org/certs/staging/letsencrypt-stg-root-x1.pem https://letsencrypt.org/certs/staging/letsencrypt-stg-root-x2.pem >"$cafile"
		curlca=(--cacert "$cafile")
	fi
	code=$(curl -sS "${curlca[@]}" -o /dev/null -w '%{http_code} %{http_version}' --max-time 60 \
		-H @"$hdr" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-06-18' \
		--data '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{"limit":1}}}' \
		"https://$PUBLIC/mcp")
	[[ "$code" == 200* ]] || fail "g: MCP list_sessions on https://$PUBLIC/mcp answered $code"
	ok "g: MCP list_sessions from this machine: HTTP $code"

	local bad hs
	bad=$(remote "journalctl -u mnemonic --since '$since' -o cat | grep -ciE 'panic|fatal|oom|out of memory|acme.*error|autocert.*error' || true")
	hs=$(remote "journalctl -u mnemonic --since '$since' -o cat | grep -c 'TLS handshake error' || true")
	[[ "$bad" == 0 ]] || fail "g: $bad panic/fatal/oom/acme error lines since restart: journalctl -u mnemonic --since '$since'"
	ok "g: journal since restart: 0 panic/fatal/oom/acme errors ($hs TLS handshake errors from scanners or old clients, informational)"
	ok "cutover done in $(($(date +%s) - t0)) s from restart; Mac client next: arena/cutover/mac-client.sh upgrade"
}

for s in a b c d f e g; do
	if want "$s"; then
		printf '%s step %s\n' "$(date +%H:%M:%S)" "$s"
		"step_$s"
	fi
done
true
