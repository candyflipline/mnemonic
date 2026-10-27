#!/bin/sh
# memcap-eq-settle.sh BIN DIR TAG: serve the store copy DIR under MemoryMax 8G
# with no client, wait for "cycle boot:", then push one FIXED marker line so
# every earlier hot component falls due, and stop when hot <= MAXHOT or MAX s
# pass. MEMCAP-C byte-equivalence run: the marker's uuid, sessionId and file
# name are constants (no `date`), so BASE and FIX push identical bytes; the
# scratch HOME is HOMEBASE (default /var/lib/mnu/memcap-eq-home-<TAG>).
# Setup, not timed: flock -s /var/lib/mnu/box.lock sh memcap-eq-settle.sh BIN DIR TAG
set -eu
BIN=${1:?usage: memcap-eq-settle.sh BIN DIR TAG}
DIR=${2:?usage: memcap-eq-settle.sh BIN DIR TAG}
TAG=${3:?usage: memcap-eq-settle.sh BIN DIR TAG}
OUT=${OUT:-/var/lib/mnu/out/memcap-c}
PORT=${PORT:-7931}
MAXHOT=${MAXHOT:-1}
MAX=${MAX:-2700}
MEM=${MEM:-8G}
HOMEBASE=${HOMEBASE:-/var/lib/mnu/memcap-eq-home-$TAG}
mkdir -p "$OUT"
CFG=$(mktemp -d)
"$BIN" token new --author uplbox > "$CFG/token.out"
{
	printf 'data_dir = "%s"\nlisten = "127.0.0.1:%s"\npush_listen = "127.0.0.1:%s"\n' "$DIR" "$PORT" $((PORT + 1))
	tail -n +2 "$CFG/token.out"
} > "$CFG/server.toml"
LOG=$OUT/settle-$TAG.log
systemd-run --scope -q -p MemoryMax=$MEM -p MemorySwapMax=0 env GOMAXPROCS=4 "$BIN" serve --config "$CFG/server.toml" > "$LOG" 2>&1 </dev/null &
PID=$!
echo "$PID" > "$OUT/settle-$TAG.pid"
trap 'kill "$PID" 2>/dev/null || true' EXIT
until grep -q "cycle boot:" "$LOG"; do kill -0 "$PID"; sleep 1; done
H=$HOMEBASE
mkdir -p "$H/.claude/projects/settle"
head -1 "$CFG/token.out" > "$CFG/token"
chmod 600 "$CFG/token"
printf 'server = "http://127.0.0.1:%s"\ntoken_file = "%s"\nmachine = "plainbox"\n' $((PORT + 1)) "$CFG/token" > "$CFG/client.toml"
printf '{"type":"user","uuid":"00000000-0000-4000-8000-0d06f00d0001","sessionId":"00000000-0000-4000-8000-0d06f00d0001","timestamp":"2026-10-26T12:00:00Z","cwd":"/settle","version":"2.0.0","parentUuid":null,"message":{"role":"user","content":"settle marker"}}\n' > "$H/.claude/projects/settle/settle-memcap-c.jsonl"
HOME=$H timeout 120 "$BIN" push --config "$CFG/client.toml" | tail -1
end=$(($(date +%s) + MAX))
rc=1
while [ "$(date +%s)" -lt "$end" ] && kill -0 "$PID" 2>/dev/null; do
	h=$(grep -o 'hot=[0-9]*' "$LOG" | tail -1 | cut -d= -f2)
	if [ -n "$h" ] && [ "$h" -le "$MAXHOT" ]; then
		rc=0
		break
	fi
	sleep 5
done
kill "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true
trap - EXIT
grep 'cycle ' "$LOG" | sed -E 's/^[0-9/]+ //' | cut -c1-260
echo "settle_rc $rc"
exit $rc
