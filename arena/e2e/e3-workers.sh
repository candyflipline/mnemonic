#!/usr/bin/env bash
# Runs done-predicate 2a-2e of traces substrate v0 on a real corpus beside a
# live bot, from prebuilt binaries. Needs the owner's go (E3_GO=1).
#
#   E3_GO=1 BIN_DIR=/var/lib/mnemonic-e3/bin RAW=/var/lib/mnemonic-e3/raw WORK=/var/lib/mnemonic-e3/run-<stamp> e3-workers.sh
#
# BIN_DIR holds a, check, e2e, seed, gen, oracle. RAW is a raw tree (author=*/... directly under it, or under RAW/raw).
# run.sh and lib.sh sit beside this script. E3_NOUNIT=1 runs without systemd-run.
# Under WORK it writes src/ (the claude and codex sources unpacked from RAW), arena/ (laid out like
# arena-fixture, raw/ made by seed -full from src/ so 2c and 2d can cut the same sources in halves),
# oracle/, tmp/ and log/. log/lines.txt holds the five lines.
set -u -o pipefail

[ "${E3_GO:-}" = 1 ] || { echo "e3-workers: refusing to run without E3_GO=1 (the owner's go)" >&2; exit 2; }
: "${BIN_DIR:?set BIN_DIR}" "${RAW:?set RAW}" "${WORK:?set WORK}"
abs() { (cd "$1" 2>/dev/null && pwd) || { echo "e3-workers: $1 is not a directory" >&2; return 1; }; }
BIN_DIR=$(abs "$BIN_DIR") || exit 2
RAW=$(abs "$RAW") || exit 2
mkdir -p "$WORK" && WORK=$(abs "$WORK") || exit 2
HERE=$(cd "$(dirname "$0")" && pwd)
for b in a check e2e seed gen oracle; do
	[ -x "$BIN_DIR/$b" ] || { echo "e3-workers: $BIN_DIR/$b is not an executable" >&2; exit 2; }
done
[ -x "$HERE/run.sh" ] && [ -r "$HERE/lib.sh" ] || { echo "e3-workers: run.sh and lib.sh must sit beside this script" >&2; exit 2; }

if [ -z "${E3_IN_UNIT:-}" ] && [ -z "${E3_NOUNIT:-}" ]; then
	exec systemd-run --unit=mnemonic-e3 --collect --wait --pipe \
		--property=CPUWeight=10 --property=Nice=10 --property=MemoryMax=8G \
		--property=OOMScoreAdjust=800 --property=IOWeight=10 \
		--setenv=E3_GO=1 --setenv=E3_IN_UNIT=1 --setenv=BIN_DIR="$BIN_DIR" --setenv=RAW="$RAW" --setenv=WORK="$WORK" \
		--setenv=GOMAXPROCS=6 --setenv=E3_STAGE_MEM="${E3_STAGE_MEM:-3GiB}" --setenv=E3_CLUSTER_MEM="${E3_CLUSTER_MEM:-1400MiB}" \
		"$HERE/$(basename "$0")"
fi

STAGE_MEM=${E3_STAGE_MEM:-3GiB}
CLUSTER_MEM=${E3_CLUSTER_MEM:-1400MiB}
export GOMAXPROCS=${GOMAXPROCS:-6}
ARENA=$WORK/arena
SRC=$WORK/src
LOG=$WORK/log
for d in arena src oracle oracle2 tmp log; do
	[ -e "$WORK/$d" ] && { echo "e3-workers: $WORK/$d exists; use a fresh WORK" >&2; exit 2; }
done
mkdir -p "$ARENA/check" "$SRC" "$WORK/tmp" "$LOG"

RAWDIR=$RAW
[ -d "$RAW/raw" ] && RAWDIR=$RAW/raw

log() { echo "e3-workers: $*" | tee -a "$LOG/stages.txt" >&2; }
stage() {
	local name=$1 start rc
	shift
	start=$(date +%s)
	log "$name start"
	"$@"
	rc=$?
	log "$name rc=$rc in $(($(date +%s) - start))s"
	return $rc
}
die() {
	log "FAILED at $1; logs in $LOG"
	exit 1
}

run_go() { env GOMEMLIMIT="$STAGE_MEM" "$@"; }

oracle_masks() {
	local set seed
	for set in golden hidden; do
		seed=1
		[ $set = hidden ] && seed=2
		run_go "$BIN_DIR/gen" -root "$WORK/oracle" -oracle "$BIN_DIR/oracle" -seed $seed -out "$ARENA/$set" || return 1
		run_go "$BIN_DIR/oracle" answer -root "$WORK/oracle2" <"$ARENA/$set/calls.jsonl" >"$WORK/$set.run2.jsonl" || return 1
		"$BIN_DIR/check" masks -set $set -a "$ARENA/$set/expected.jsonl" -b "$WORK/$set.run2.jsonl" -out "$ARENA/check/masks.json" || return 1
	done
}

stage unpack run_go "$BIN_DIR/e2e" unpack -raw "$RAWDIR" -src "$SRC" >"$LOG/unpack.log" 2>&1 || die unpack
stage seed run_go "$BIN_DIR/seed" -src "$SRC" -full "$ARENA/raw" 2>"$LOG/seed.log" || die seed
stage oracle-build run_go "$BIN_DIR/oracle" build -raw "$ARENA/raw" -root "$WORK/oracle" 2>"$LOG/oracle-build.log" || die oracle-build
stage oracle-build-2 run_go "$BIN_DIR/oracle" build -raw "$ARENA/raw" -root "$WORK/oracle2" 2>"$LOG/oracle-build-2.log" || die oracle-build-2
stage gen-and-masks oracle_masks 2>"$LOG/gen.log" || die gen-and-masks
rm -rf "$WORK/oracle2"

ENVLABEL=${E3_ENVLABEL:-loyal-workers, real corpus} E2E_TIMEOUT=${E3_TIMEOUT:-3h} WAIT_UP_S=${E3_WAIT_UP_S:-900} WAIT_EXIT_S=${E3_WAIT_EXIT_S:-600} \
	PREWARM_WAIT_S=${E3_PREWARM_WAIT_S:-1800} GOMEMLIMIT=$CLUSTER_MEM \
	ARENA="$ARENA" SRC="$SRC" BIN_DIR="$BIN_DIR" LOG_DIR="$LOG/e2e" TMPDIR="$WORK/tmp" \
	stage run.sh "$HERE/run.sh" >"$LOG/lines.txt" 2>"$LOG/run.stderr"
RC=$?

peak() {
	local v cg
	v=$(systemctl show mnemonic-e3.service -p MemoryPeak --value 2>/dev/null)
	if ! [ "${v:-}" -ge 0 ] 2>/dev/null; then
		cg=/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup 2>/dev/null)/memory.peak
		v=$(cat "$cg" 2>/dev/null)
	fi
	if [ "${v:-}" -ge 0 ] 2>/dev/null; then echo "$((v / 1048576)) MiB"; else echo "n/a (not in a unit)"; fi
}

echo "---- the five lines"
cat "$LOG/lines.txt"
echo "---- stages"
cat "$LOG/stages.txt" "$LOG/e2e/steps.txt" 2>/dev/null
echo "---- du"
du -sh "$RAWDIR" "$SRC" "$ARENA/raw" "$WORK/oracle" "$WORK/oracle/raw" "$WORK/oracle/events" "$WORK/oracle/cache" "$LOG" 2>/dev/null | sed "s#$WORK/##"
echo "per step, KiB (the object store, the keeper root of 2c/2d, the keeper local dir, the reader caches), as each step ended:"
cat "$LOG/e2e/du.txt" 2>/dev/null
echo "---- unit peak memory: $(peak)"
exit $RC
