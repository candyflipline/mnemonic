#!/bin/sh
# p4-cycle.sh BIN DIR TAG — one lag.sh run (ROWS-row probes) on DIR under
# MemoryMax 8G, sampling the serve's VmHWM and its scope's memory.events
# oom_kill every 2 s. PROFILE=1 also records `perf record -g` for 60 s and
# `strace -c -f` for 30 s on the serve once the first probe row prints
# (an unreported run: the tracers slow it down). Hold the box lock:
#   flock /var/lib/mnu/box.lock sh p4-cycle.sh BIN DIR TAG
# Env: as lag.sh (N 10, ROWS 10, SPACING 15, PORT 7841, PUSH_PORT 7842), OUT.
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
