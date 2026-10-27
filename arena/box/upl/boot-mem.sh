#!/bin/sh
set -eu
BIN=${1:?usage: boot-mem.sh BIN DIR TAG}
DIR=${2:?usage: boot-mem.sh BIN DIR TAG}
TAG=${3:?usage: boot-mem.sh BIN DIR TAG}
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-/var/lib/mnu/out/p4b2g}
PORT=${PORT:-7871}
MEM=${MEM:-8G}
MAX=${MAX:-900}
HOLD=${HOLD:-30}
mkdir -p "$OUT"
CFG=$(mktemp -d)
"$BIN" token new --author uplbox > "$CFG/token.out"
{
	printf 'data_dir = "%s"\nlisten = "127.0.0.1:%s"\npush_listen = "127.0.0.1:%s"\n' "$DIR" "$PORT" $((PORT + 1))
	tail -n +2 "$CFG/token.out"
} > "$CFG/server.toml"
LOG=$OUT/boot-$TAG.log
echo "tag $TAG bin_sha256 $(sha256sum "$BIN" | cut -c1-12) mem $MEM free_avail_gb $(free -g | awk '/^Mem:/{print $7}')"
echo "before $(sh "$HERE/hot-shape.sh" "$DIR")"
since=$(date '+%F %T')
t0=$(date +%s)
systemd-run --scope -q --unit="p4b2g-$TAG-$t0" -p MemoryMax=$MEM -p MemorySwapMax=0 env GOMAXPROCS=4 "$BIN" serve --config "$CFG/server.toml" > "$LOG" 2>&1 </dev/null &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
sleep 1
CG=/sys/fs/cgroup$(cut -d: -f3 /proc/$PID/cgroup)
peak=0 anon=0 hwm=0 oom=0 booted= until=
while kill -0 $PID 2>/dev/null; do
	p=$(cat "$CG/memory.peak" 2>/dev/null || echo 0)
	a=$(awk '$1=="anon"{print $2}' "$CG/memory.stat" 2>/dev/null || echo 0)
	w=$(awk '/^VmHWM/{print $2}' /proc/$PID/status 2>/dev/null || echo 0)
	o=$(awk '$1=="oom_kill"{print $2}' "$CG/memory.events" 2>/dev/null || echo 0)
	[ "${p:-0}" -gt "$peak" ] && peak=$p
	[ "${a:-0}" -gt "$anon" ] && anon=$a
	[ "${w:-0}" -gt "$hwm" ] && hwm=$w
	[ "${o:-0}" -gt "$oom" ] && oom=$o
	now=$(date +%s)
	if [ -z "$booted" ] && grep -q 'cycle boot:' "$LOG"; then booted=$((now - t0)); until=$((now + HOLD)); fi
	[ -n "$until" ] && [ "$now" -ge "$until" ] && break
	[ $((now - t0)) -ge "$MAX" ] && break
	sleep 1
done
alive=0
kill -0 $PID 2>/dev/null && alive=1
kill $PID 2>/dev/null || true
wait $PID 2>/dev/null && rc=0 || rc=$?
trap - EXIT
echo "result tag $TAG booted_s ${booted:-none} alive_at_stop $alive serve_rc $rc memory_peak_bytes $peak anon_max_bytes $anon vmhwm_kb $hwm oom_kill $oom"
grep -E 'cycle boot:|panic|fatal|out of memory' "$LOG" | sed -E 's/^[0-9/]+ //' | cut -c1-240 || true
echo "kernel_oom_lines $(journalctl -k --since "$since" --no-pager | grep -c 'Killed process' || true)"
journalctl -k --since "$since" --no-pager | grep 'Killed process' | cut -c1-220 || true
echo "after $(sh "$HERE/hot-shape.sh" "$DIR")"
