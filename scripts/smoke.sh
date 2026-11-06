#!/usr/bin/env bash
# Usage: scripts/smoke.sh <mnemonic_*.tar.gz>
# Unpacks a release archive, runs `mnemonic local` over the testdata fixture in a
# throwaway HOME, and asks it for the fixture session over MCP.
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 <archive.tar.gz>" >&2; exit 2; }
archive=$1
[ -f "$archive" ] || { echo "FAIL: no such archive: $archive" >&2; exit 2; }
archive=$(cd "$(dirname "$archive")" && pwd)/$(basename "$archive")

repo=$(cd "$(dirname "$0")/.." && pwd)
fixture=$repo/testdata/fixtures/claude-code/session.jsonl
session=7c666c01-d38e-4658-8650-854ffb5b626e
want=session:$(printf %s "$session" | base64 | tr -d '=\n')

work=$(mktemp -d)
unset XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME XDG_CACHE_HOME XDG_RUNTIME_DIR
export HOME=$work/home
mkdir -p "$HOME/.claude/projects/demo" "$work/bin"
cp "$fixture" "$HOME/.claude/projects/demo/session.jsonl"
tar -xzf "$archive" -C "$work/bin" mnemonic
bin=$work/bin/mnemonic

pid=
cleanup() {
	status=$?
	[ -z "$pid" ] || { kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }
	if [ "$status" -ne 0 ]; then
		echo "--- server log" >&2
		cat "$work/local.log" >&2 2>/dev/null || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

echo "archive $(basename "$archive")"
"$bin" version

port=
for _ in $(seq 1 20); do
	p=$((20000 + (RANDOM % 40000)))
	if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
		port=$p
		break
	fi
done
[ -n "$port" ] || fail "no free port found"

"$bin" local --listen "127.0.0.1:$port" >"$work/local.log" 2>&1 &
pid=$!
for _ in $(seq 1 150); do
	grep -q "listening on" "$work/local.log" && break
	kill -0 "$pid" 2>/dev/null || fail "mnemonic local exited before listening"
	sleep 0.2
done
grep -q "listening on" "$work/local.log" || fail "mnemonic local did not log 'listening on' within 30s"

rpc() {
	curl -fsS -X POST "http://127.0.0.1:$port/mcp" \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-H 'Mcp-Protocol-Version: 2025-06-18' \
		-d "$1" >"$work/resp.json" || fail "POST /mcp failed for $2"
}

rpc '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' initialize

for _ in $(seq 1 50); do
	rpc '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"inspect sample project","n_hits":5}}}' search_sessions
	grep -q "\"id\":\"$want\"" "$work/resp.json" && break
	sleep 0.5
done
grep -q "\"id\":\"$want\"" "$work/resp.json" || { cat "$work/resp.json" >&2; fail "search_sessions response does not name $want (base64 of $session)"; }

echo "PASS: $(basename "$archive") search_sessions named fixture session $session as $want"
