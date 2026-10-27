#!/bin/sh
# hot-settle.sh BIN DIR: serve DIR under MemoryMax MEM (8G) with no client
# until a cycle logs hot <= MAXHOT (default 1) or MAX s pass (default 1800),
# then stop it. Once serve listens it pushes one marker line from a scratch
# HOME: a source is due to settle when it is settleSource older than the newest
# source, so the marker makes every earlier hot component due, and the settle
# cycles (every 5 min, parsing nothing) settle up to GOMAXPROCS shards each.
# Resets a store to cold before lag.sh HOT mode.
# Setup, not timed: flock -s /var/lib/mnu/box.lock sh hot-settle.sh BIN DIR
set -eu
BIN=${1:?usage: hot-settle.sh BIN DIR}
DIR=${2:?usage: hot-settle.sh BIN DIR}
OUT=${OUT:-/var/lib/mnu/out/p4b}
PORT=${PORT:-7853}
MAXHOT=${MAXHOT:-1}
MAX=${MAX:-1800}
MEM=${MEM:-8G}
CFG=$(mktemp -d)
"$BIN" token new --author uplbox > "$CFG/token.out"
{
	printf 'data_dir = "%s"\nlisten = "127.0.0.1:%s"\npush_listen = "127.0.0.1:%s"\n' "$DIR" "$PORT" $((PORT + 1))
	tail -n +2 "$CFG/token.out"
} > "$CFG/server.toml"
LOG=$OUT/settle-$(basename "$DIR")-$(date +%s).log
systemd-run --scope -q -p MemoryMax=$MEM -p MemorySwapMax=0 env GOMAXPROCS=4 "$BIN" serve --config "$CFG/server.toml" > "$LOG" 2>&1 </dev/null &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
until grep -q "cycle boot:" "$LOG"; do kill -0 $PID; sleep 1; done
H=$CFG/home
mkdir -p "$H/.claude/projects/settle"
head -1 "$CFG/token.out" > "$CFG/token"
chmod 600 "$CFG/token"
printf 'server = "http://127.0.0.1:%s"\ntoken_file = "%s"\nmachine = "plainbox"\n' $((PORT + 1)) "$CFG/token" > "$CFG/client.toml"
printf '{"type":"user","uuid":"00000000-0000-4000-8000-%012d","sessionId":"00000000-0000-4000-8000-00000000005e","timestamp":"2026-10-26T12:00:00Z","cwd":"/settle","version":"2.0.0","parentUuid":null,"message":{"role":"user","content":"settle marker"}}\n' "$(date +%s)" > "$H/.claude/projects/settle/settle-$(date +%s).jsonl"
HOME=$H timeout 120 "$BIN" push --config "$CFG/client.toml" | tail -1
end=$(($(date +%s) + MAX))
rc=1
while [ "$(date +%s)" -lt "$end" ] && kill -0 $PID 2>/dev/null; do
	h=$(grep -o 'hot=[0-9]*' "$LOG" | tail -1 | cut -d= -f2)
	if [ -n "$h" ] && [ "$h" -le "$MAXHOT" ]; then
		rc=0
		break
	fi
	sleep 5
done
kill $PID 2>/dev/null || true
wait $PID 2>/dev/null || true
trap - EXIT
grep 'cycle ' "$LOG" | sed -E 's/^[0-9/]+ //' | cut -c1-260
echo "settle_rc $rc"
exit $rc
