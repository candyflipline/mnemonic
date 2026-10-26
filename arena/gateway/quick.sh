#!/bin/sh
# quick.sh DIR: from the Mac, drive the spike serve on mnemonic-p1 through the
# quick tunnels in DIR/hosts (lines "name https://x.trycloudflare.com") with
# the token in DIR/token. Rows: check<TAB>target<TAB>result. Spike tooling.
#   DRV   caddydrv built for the Mac   BIN  mnemonic built for the Mac
set -u
D=${1:?usage: quick.sh DIR}
DRV=${DRV:?}
BIN=${BIN:?}
TOK=$(cat "$D/token")
row() { printf '%s\t%s\t%s\n' "$1" "$2" "$3"; }
host() { awk -v n="$1" '$1==n{print $2}' "$D/hosts"; }
for n in push push-h2; do
	h=$(host "$n"); [ -n "$h" ] || continue
	for i in 1 2; do
		out=$("$DRV" lat -server "$h" -token "$TOK" -machine "q$i" -n 1 2>&1 | head -1 | cut -c1-200)
		row "drv-hello-$i" "$n" "$out"
		sleep 2
	done
	for ct in application/grpc application/grpc+proto application/octet-stream; do
		row "curl-post-$ct" "$n" "$(curl -s -o /dev/null -w '%{http_code} h%{http_version}' --http2 -X POST -H "content-type: $ct" -H 'te: trailers' --data-binary x "$h/mnemonic.push.v1.Push/Sync")"
	done
	mkdir -p "$D/home-$n/.claude/projects/p" "$D/home-$n/.mnemonic"
	printf '{"t":"spike","i":1}\n{"t":"spike","i":2}\n' > "$D/home-$n/.claude/projects/p/s.jsonl"
	printf 'server = "%s"\ntoken_file = "%s"\nmachine = "client-%s"\n' "$h" "$D/token" "$n" > "$D/client-$n.toml"
	HOME=$D/home-$n "$BIN" push --config "$D/client-$n.toml" > "$D/client-$n.out" 2>&1; rc=$?; out=$(tail -2 "$D/client-$n.out" | tr "\n" " " | cut -c1-160)
	row "client-push" "$n" "rc=$rc $out"
done
h=$(host http)
mcp() { curl -s -H 'mcp-protocol-version: 2025-06-18' -w '\nHTTP %{http_code} h%{http_version}' -H "authorization: Bearer $TOK" -H 'content-type: application/json' -H 'accept: application/json, text/event-stream' --data "$1" "$h/mcp"; }
r=$(mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"spike","version":"0"}}}')
row mcp-initialize http "$(echo "$r" | tail -1) server=$(echo "$r" | head -1 | jq -r '.result.serverInfo.name? // .error.message? // "?"' 2>/dev/null)"
r=$(mcp '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
row mcp-tools-list http "$(echo "$r" | tail -1) tools=$(echo "$r" | head -1 | jq -r '.result.tools|length' 2>/dev/null)"
r=$(mcp '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_sessions","arguments":{"start_datetime":"2026-10-01T00:00:00Z","end_datetime":"2026-11-01T00:00:00Z"}}}')
row mcp-list-sessions http "$(echo "$r" | tail -1) isError=$(echo "$r" | head -1 | jq -r '.result.isError? // .error.message? // "false"' 2>/dev/null) bytes=$(echo "$r" | head -1 | wc -c | tr -d ' ')"
r=$(curl -s -o /dev/null -w '%{http_code}' -H 'content-type: application/json' --data '{"jsonrpc":"2.0","id":4,"method":"tools/list"}' "$h/mcp")
row mcp-no-token http "HTTP $r"
