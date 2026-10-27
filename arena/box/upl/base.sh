#!/bin/sh
set -eu

BIN=${1:?usage: base.sh BIN}
BOX=${BOX:-/var/lib/mnu}
OUT=${OUT:-$BOX/out/base}
DEADLINE=${DEADLINE:-4500}
HERE=$(dirname "$0")
CFG=$BOX/base.cfg/server.toml
LOG=$OUT/serve.log
mkdir -p "$OUT"

ts() { date -u +%FT%TZ; }

echo "$(ts) storesha source"
JOBS=8 sh "$HERE/storesha.sh" "$BOX" > "$OUT/storesha.raw"
echo "$(ts) storesha base"
JOBS=8 sh "$HERE/storesha.sh" "$BOX/base" > "$OUT/storesha.base.pre"
cmp "$OUT/storesha.raw" "$OUT/storesha.base.pre"
echo "storesha_equal	$(wc -l < "$OUT/storesha.raw") files"

[ -e "$BOX/base/index" ] && { echo "base/index already exists; one boot only" >&2; exit 3; }
: > "$LOG"
START=$(date +%s.%N)
echo "$(ts) boot"
systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 env GOMAXPROCS=4 "$BIN" serve --config "$CFG" > "$LOG" 2>&1 &
PID=$!
echo "$PID" > "$OUT/serve.pid"
trap 'kill "$PID" 2>/dev/null || true' EXIT

HWM=0
EVENTS=
VERDICT=timeout
BOOTLINE=
QUIETLINE=
while :; do
	NOW=$(date +%s.%N)
	if [ ! -d "/proc/$PID" ]; then VERDICT=died; break; fi
	h=$(awk '/^VmHWM/{print $2}' "/proc/$PID/status" 2>/dev/null || true)
	[ -n "$h" ] && HWM=$h
	cg=$(sed -n 's#^0::#/sys/fs/cgroup#p' "/proc/$PID/cgroup" 2>/dev/null || true)
	[ -n "$cg" ] && [ -r "$cg/memory.events" ] && EVENTS=$(tr '\n' ' ' < "$cg/memory.events")
	if [ -z "$BOOTLINE" ]; then
		BOOTLINE=$(grep -m1 'cycle boot:' "$LOG" || true)
		[ -n "$BOOTLINE" ] && BOOTWALL=$(echo "$NOW - $START" | bc)
	fi
	if [ -n "$BOOTLINE" ]; then
		QUIETLINE=$(grep -E 'cycle [a-z]+: sources unchanged=[0-9]+ resumed=0 fresh=0 reparsed=0 failed=[0-9]+ stale=0;' "$LOG" | grep -v 'cycle boot:' | head -1 || true)
		if [ -n "$QUIETLINE" ]; then
			VERDICT=quiet
			QUIETWALL=$(echo "$NOW - $START" | bc)
			break
		fi
	fi
	if [ "$(echo "$NOW - $START > $DEADLINE" | bc)" = 1 ]; then break; fi
	sleep 2
done

echo "verdict	$VERDICT"
echo "pid	$PID"
if [ -d "/proc/$PID" ]; then
	h=$(awk '/^VmHWM/{print $2}' "/proc/$PID/status" || true)
	[ -n "$h" ] && HWM=$h
	cg=$(sed -n 's#^0::#/sys/fs/cgroup#p' "/proc/$PID/cgroup" || true)
	[ -n "$cg" ] && [ -r "$cg/memory.events" ] && EVENTS=$(tr '\n' ' ' < "$cg/memory.events")
	echo "exe	$(readlink "/proc/$PID/exe")"
	echo "$(ts) SIGTERM $PID"
	kill -TERM "$PID"
	i=0
	while [ -d "/proc/$PID" ] && [ "$i" -lt 150 ]; do sleep 2; i=$((i + 1)); done
	[ -d "/proc/$PID" ] && echo "still alive after 300 s" >&2
fi
trap - EXIT
echo "boot_cycle_line	$BOOTLINE"
echo "boot_wall_s_to_line	${BOOTWALL:-none}"
echo "quiet_cycle_line	$QUIETLINE"
echo "quiet_wall_s	${QUIETWALL:-none}"
echo "VmHWM_kB	$HWM"
echo "memory_events	$EVENTS"
echo "cycle_lines	$(grep -c 'cycle ' "$LOG" || true)"
echo "failed_nonzero_cycle_lines	$(grep 'cycle ' "$LOG" | grep -vc 'failed=0 ' || true)"
echo "err_lines	$(grep -c -E 'err=[^<]|panic|fatal|level=(error|ERROR)' "$LOG" || true)"
echo "log_lines	$(wc -l < "$LOG")"
dmesg -T 2>/dev/null | tail -200 | grep -c -i 'out of memory' | sed 's/^/dmesg_oom_lines_last200	/' || true

JOBS=8 sh "$HERE/storesha.sh" "$BOX/base" > "$OUT/storesha.base.post"
cmp "$OUT/storesha.raw" "$OUT/storesha.base.post" && echo "storesha_equal_after_boot	yes"
echo "index_files	$(find "$BOX/base/index" -type f | wc -l)"
echo "index_bytes	$(du -sb "$BOX/base/index" | cut -f1)"

if [ "$VERDICT" = quiet ]; then
	chmod -R a-w "$BOX/base"
	echo "base chmod a-w: done"
fi
echo "$(ts) done"
