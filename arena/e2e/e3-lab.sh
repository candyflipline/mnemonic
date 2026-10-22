#!/usr/bin/env bash
# NEEDS THE OWNER'S GO: never run without it. It reads the production corpus
# on the server and loads the box, so it refuses unless E3_GO=1 is set by the
# owner's say-so. Written, not run.
#
# On the server lab (/var/lib/mnemonic-lab, a lab copy of /var/lib/mnemonic) it:
#   1. builds oracle, gen, check (DuckDB, cgo) and a, e2e (no cgo) into the lab;
#   2. copies the corpus raw into the lab and has the oracle ingest the copy;
#   3. draws two call sets with gen from the oracle's index, and records the
#      oracle's own run-to-run differences as masks;
#   4. runs the distributed candidate (1 keeper + 3 readers on a file store
#      under the lab) over the same raw copy and diffs both rungs against the
#      oracle with the e2e driver.
# It runs inside a transient unit (MemoryMax=8G, Nice=10) and writes only
# under the lab; `systemctl stop mnemonic-e3` deletes the unit. Prints one
# line per rung with equal/total calls, and exits non-zero on any difference.
set -u -o pipefail

LAB=${LAB:-/var/lib/mnemonic-lab}
PROD=${PROD:-/var/lib/mnemonic}
REPO=${REPO:?set REPO to the lane/traces checkout on the server}
SEED=${SEED:-1}
export TENANT=e3

[ "${E3_GO:-}" = 1 ] || { echo "e3-lab: refusing to run without E3_GO=1 (the owner's go)" >&2; exit 2; }
case "$LAB/" in
"$PROD"/*) echo "e3-lab: LAB must not be under $PROD" >&2; exit 2 ;;
esac
[ -d "$LAB" ] || { echo "e3-lab: $LAB does not exist" >&2; exit 2; }
[ -d "$PROD/raw" ] || { echo "e3-lab: $PROD/raw does not exist" >&2; exit 2; }

if [ -z "${E3_IN_UNIT:-}" ]; then
	exec systemd-run --unit=mnemonic-e3 --collect --wait --pipe \
		--property=MemoryMax=8G --property=Nice=10 \
		--setenv=E3_GO=1 --setenv=E3_IN_UNIT=1 --setenv=REPO="$REPO" --setenv=LAB="$LAB" \
		--setenv=PROD="$PROD" --setenv=SEED="$SEED" --setenv=GOMAXPROCS=4 --setenv=GOMEMLIMIT=1500MiB \
		"$(readlink -f "$0")"
fi

export GOMAXPROCS=${GOMAXPROCS:-4} GOMEMLIMIT=${GOMEMLIMIT:-1500MiB}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
T=$LAB/e3-$STAMP
BIN=$T/bin
LOGS=$T/log
ARENA=$T/arena
mkdir -p "$BIN" "$LOGS" "$ARENA/check" "$T/src"
ln -sfn "$T" "$LAB/e3-current"
PIDS=""
E2E=$BIN/e2e
# shellcheck source=./lib.sh
. "$(dirname "$(readlink -f "$0")")/lib.sh"

# shellcheck disable=SC2329
cleanup() {
	trap - EXIT INT TERM
	for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
	for p in $PIDS; do wait "$p" 2>/dev/null; done
}
trap cleanup EXIT
trap 'exit 130' INT TERM

echo "e3-lab: building" >&2
(
	cd "$REPO" || exit 1
	go build -o "$BIN/oracle" ./arena/oracle &&
		go build -o "$BIN/gen" ./arena/gen &&
		go build -o "$BIN/check" ./arena/check &&
		CGO_ENABLED=0 go build -o "$BIN/a" ./arena/a &&
		CGO_ENABLED=0 go build -o "$BIN/e2e" ./arena/e2e
) || { echo "e3-lab: build failed" >&2; exit 1; }

echo "e3-lab: copying raw into the lab" >&2
cp -a "$PROD/raw" "$T/src/raw" || exit 1
"$BIN/oracle" build -raw "$T/src" -root "$T/oracle" || exit 1
rm -rf "${T:?}/src"

for set in golden hidden; do
	seed=$SEED
	[ $set = hidden ] && seed=$((SEED + 1))
	"$BIN/gen" -root "$T/oracle" -oracle "$BIN/oracle" -seed "$seed" -out "$ARENA/$set" || exit 1
	"$BIN/oracle" answer -root "$T/oracle" <"$ARENA/$set/calls.jsonl" >"$T/$set.again.jsonl" || exit 1
	"$BIN/check" masks -set $set -a "$ARENA/$set/expected.jsonl" -b "$T/$set.again.jsonl" -out "$ARENA/check/masks.json" || exit 1
done

STORE=$T/store
mkdir -p "$STORE"
cluster_up e3 "$STORE" "$T/oracle" "" || { echo "e3-lab: cluster did not start ($(grep -h -m1 '^panic:' "$LOGS"/*.log))" >&2; exit 1; }
"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || exit 1

rc=0
for rung in whole partial; do
	out=$(drive -rung $rung) || rc=1
	echo "e3: $rung $(ratio "$(field "$out" equal)" "$(field "$out" total)") against the oracle, server lab"
done
exit $rc
