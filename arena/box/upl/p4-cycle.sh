#!/bin/sh
set -eu
BIN=${1:?usage: p4-cycle.sh BIN DIR TAG}
DIR=${2:?usage: p4-cycle.sh BIN DIR TAG}
TAG=${3:?usage: p4-cycle.sh BIN DIR TAG}
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-/var/lib/mnu/out/p4}
mkdir -p "$OUT"
L=$OUT/run-$TAG
export OUT TAG N=${N:-10} ROWS=${ROWS:-10} SPACING=${SPACING:-15} PORT=${PORT:-7841} PUSH_PORT=${PUSH_PORT:-7842}
export SERVE_WRAP="systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0"
sh "$HERE/lag.sh" "$BIN" "$DIR" > "$L.lag.txt" 2>&1 &
LAG=$!
trap 'kill $LAG 2>/dev/null || true' EXIT
CFG=$OUT/base-$(basename "$DIR")-lag-$TAG-cfg/server.toml
pid=
hwm=0
oom=
profiled=0
while kill -0 $LAG 2>/dev/null; do
	[ -n "$pid" ] && kill -0 "$pid" 2>/dev/null || pid=$(pgrep -f "serve --config $CFG" | head -1 || true)
	if [ -n "$pid" ] && [ -r /proc/$pid/status ]; then
		h=$(awk '/^VmHWM/{print $2}' /proc/$pid/status 2>/dev/null || echo 0)
		[ "${h:-0}" -gt "$hwm" ] && hwm=$h
		cg=$(cut -d: -f3 /proc/$pid/cgroup 2>/dev/null || true)
		[ -n "$cg" ] && [ -r "/sys/fs/cgroup$cg/memory.events" ] && oom=$(awk '/^oom_kill /{print $2}' "/sys/fs/cgroup$cg/memory.events")
		if [ "${PROFILE:-0}" = 1 ] && [ $profiled = 0 ] && grep -q '^probe' "$L.lag.txt"; then
			profiled=1
			perf record -g -F 499 -p "$pid" -o "$L.perf.data" -- sleep 60 > "$L.perf.log" 2>&1
			timeout -s INT 30 strace -f -c -p "$pid" -o "$L.strace-c.txt" > /dev/null 2>&1 || true
		fi
	fi
	sleep 2
done
wait $LAG && rc=0 || rc=$?
trap - EXIT
echo "lag_rc $rc vmhwm_kb_max $hwm oom_kill ${oom:-unread}" | tee "$L.mem.txt"
grep -E '^(probe|index_ms|ack_ms|cycle_s_after_quiet|cycle_s_steady|hot_steady|hot_appends|hot_files|kinds_steady|cycle_err|boot_to|warmup)' "$L.lag.txt" || true
