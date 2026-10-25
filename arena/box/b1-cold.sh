#!/bin/sh
# b1-cold.sh — lane B1: one clean cold engine build at GOMAXPROCS=4 under
# MemoryMax=8G (prod's deploy shape), du + df sampled every 10 s alongside,
# pidstat alongside, heap profile every 15 s when PERF_PROF is set (the
# driver keeps two alternating files; the newest by mtime is the latest
# sample). The store must not exist: a cold build needs an empty store and
# nothing on the box is ever deleted.
#   flock /var/lib/mnemonic-e3/box.lock arena/box/b1-cold.sh TESTBIN ROOT STORE UNIT
##   $1 TESTBIN  engine test binary carrying TestPerfCycle (in-process driver)
##   $2 ROOT     dataroot (b1/root-ra for the reported raw+archive build)
##   $3 STORE    fresh store path (under $BOX/b1/)
##   $4 UNIT     systemd scope unit name (unique per run)
## Env: OUT (default $BOX/out/b1), PERF_PROF (heap+cpu profiles dir).
## Logs hold counts and timings only. Disk guard: if / free drops below
## 10 GB the sampler kills the run at once and the log says so.
set -eu
BOX=${BOX:-/var/lib/mnemonic-e3}
TESTBIN=${1:?usage: b1-cold.sh TESTBIN ROOT STORE UNIT}
ROOT=${2:?ROOT}
STORE=${3:?STORE}
UNIT=${4:?UNIT}
OUT=${OUT:-$BOX/out/b1}
N=$(date +%s%N)
LOG=$OUT/cold-$UNIT-$N.log
DU=$OUT/cold-$UNIT-$N.du
mkdir -p "$STORE" "$OUT"
[ -z "$(ls -A "$STORE" 2>/dev/null)" ] || {
	echo "b1-cold.sh: $STORE is not empty — cold build needs a fresh store; nothing is deleted, pick a new path" >&2
	exit 1
}
: > "$LOG"
(
	while :; do
		free=$(df --output=avail -B1 / | tail -1 | tr -d ' ')
		printf '%s store_bytes=%s df_free_B=%s\n' "$(date +%s)" \
			"$(du -sb "$STORE" 2>/dev/null | cut -f1)" "$free"
		if [ "${free:-0}" -lt $((10 * 1024 * 1024 * 1024)) ]; then
			echo "DISK GUARD: free ${free}B < 10GiB — stopping the run"
			systemctl kill --kill-who=main "$UNIT" 2>/dev/null || true
			exit 1
		fi
		sleep 10
	done
) > "$DU" 2>&1 &
DUP=$!
pidstat -u -h 1 > "$LOG.pidstat" 2>&1 &
PSP=$!
RC=0
/usr/bin/time.gnu -v systemd-run --scope --unit="$UNIT" -p MemoryMax=8G \
	env GOMAXPROCS=4 PERF_ROOT="$ROOT" PERF_STORE="$STORE" \
	PERF_SETTLE=1 PERF_PROF="${PERF_PROF:-}" \
	"$TESTBIN" -test.run '^TestPerfCycle$' -test.v -test.count=1 >> "$LOG" 2>&1 || RC=$?
kill "$DUP" "$PSP" 2>/dev/null || true
{
	echo "rc=$RC"
	echo "unit_memory_peak $(cat /sys/fs/cgroup/system.slice/$UNIT.scope/memory.peak 2>/dev/null || echo '?')"
	echo "peak_du_bytes $(sed 's/.*store_bytes=//; s/ .*//' "$DU" | grep -e . | sort -n | tail -1)"
	echo "final_df_free_B $(grep df_free_B "$DU" | tail -1 | sed 's/.*df_free_B=//')"
	echo "disk_guard $(grep -c 'DISK GUARD' "$DU" || true)"
} >> "$LOG"
grep -E '^perf|^=== RUN|Maximum resident|Elapsed .wall|User time|System time|Percent of CPU|rc=|unit_memory_peak|peak_du|final_df|disk_guard|DISK GUARD' "$LOG" | tail -45
echo "log $LOG du $DU pidstat $LOG.pidstat prof ${PERF_PROF:-none}"
