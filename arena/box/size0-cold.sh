#!/bin/sh
# size0-cold.sh — lane S0 (d): one cold raw+archive engine build at
# GOMAXPROCS=4 under MemoryMax=8G, heap profile snapshot every 10 s (the
# driver keeps the last two, so the newest is the peak; the surviving newest
# after an OOM kill is the peak it died at), du + df every 10 s alongside.
# Counts, sizes and timings only. Run under the box lock:
#   flock /var/lib/mnemonic-e3/box.lock arena/box/size0-cold.sh TESTBIN
## Inputs
##   $1  TESTBIN  the engine test binary carrying the in-process cold driver
##                (internal/index perf/cold scratch mode, PERF_MODE=cold)
##   $2  ROOT     dataroot with raw/ and events/ (default $BOX/size0/root)
##   $3  STORE    the store to build fresh (default $BOX/size0/store)
## Env: OUT (default $BOX/out) — log, .du timeline, pidstat, heap profiles.
set -eu
BOX=/var/lib/mnemonic-e3
TESTBIN=${1:?usage: size0-cold.sh TESTBIN [ROOT] [STORE]}
ROOT=${2:-$BOX/size0/root}
STORE=${3:-$BOX/size0/store}
OUT=${OUT:-$BOX/out}
N=$(date +%s%N)
LOG=$OUT/size0-cold-$N.log
DU=$OUT/size0-cold-$N.du
rm -rf "$STORE"
mkdir -p "$STORE" "$OUT"
: > "$LOG"
( while :; do
    printf '%s store_bytes=%s df_free=%s\n' "$(date +%s)" \
      "$(du -sb "$STORE" 2>/dev/null | cut -f1)" \
      "$(df --output=avail -B1 "$BOX" | tail -1 | tr -d ' ')"
    sleep 10
  done ) > "$DU" 2>&1 &
DUP=$!
pidstat -u -h 1 > "$LOG.pidstat" 2>&1 &
PSP=$!
RC=0
/usr/bin/time.gnu -v systemd-run --scope --unit=size0-cold-$N -p MemoryMax=8G \
  env GOMAXPROCS=4 PERF_ROOT="$ROOT" PERF_STORE="$STORE" PERF_MODE=cold PERF_PROF="$OUT" \
  "$TESTBIN" -test.run '^TestPerfCycle$' -test.v -test.count=1 >> "$LOG" 2>&1 || RC=$?
kill "$DUP" "$PSP" 2>/dev/null || true
echo "rc=$RC" >> "$LOG"
echo "peak_du_bytes $(sed 's/.*store_bytes=//; s/ .*//' "$DU" | sort -n | tail -1)"
echo "final_df_free $(tail -1 "$DU" | tr ' ' '\n' | grep df_free | cut -d= -f2)"
grep -E '^perf|^heap sample|^=== RUN|Maximum resident|Elapsed .wall|rc=' "$LOG" | tail -40
echo "log $LOG du $DU pidstat $LOG.pidstat heap_profiles $OUT/heap-*.pprof"
