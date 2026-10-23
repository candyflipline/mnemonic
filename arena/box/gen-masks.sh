#!/bin/sh
# gen-masks.sh — the real-corpus call sets, the masks, and the oracle self-check.
#
## Inputs
##   $1  BIN  the binary under test; its sibling binaries from the same tree
##            (gen, check) are found next to it, per arena/README.md's build.
## Requires: ingest-timed.sh ran first for this BIN — $BOX/cold-1 and
##           $BOX/cold-3 are two full stores of the same corpus (cold-3 is the
##           oracle root gen draws from, cold-1 the second store the masks
##           diff needs).
## Env: BOX (default /var/lib/mnemonic-e3). SEED_GOLDEN/SEED_HIDDEN (default
##      1/2). SKIP_CHECK=1 skips the self-check. CHECK_WRAPPER overrides the
##      -bin wrapper (default: $BIN-dir/wrapper-<basename>, else one is written).
#
# - gen: `gen -root $BOX/cold-3 -oracle BIN -seed 1 -out $BOX/arena/golden`,
#   then seed 2 -out $BOX/arena/hidden (calls.jsonl + expected.jsonl each).
# - masks: the same calls answered from the second store via `BIN answer`,
#   diffed against the first store's answers with `check masks`, both sets
#   merged into $BOX/arena/check/masks.json.
# - self-check: `check -bin WRAPPER -name oracle-self -arena $BOX/arena -skip
#   incremental,crash,appendcrash`; the wrapper pins GOMAXPROCS=16 so check's
#   clean ingest sees the env of the timed runs. Its clean phase is one more
#   full ingest into a scratch store. Per-tool N/N parsed from the scorecard
#   into $BOX/out/oracle-self-summary.txt; scorecard JSON in
#   $BOX/arena/runs/oracle-self-*.json.
#
# Box layout note: check's clean phase runs `ingest -root <arena>/raw`, so the
# arena holds a raw/ DATAROOT whose raw/raw symlinks to the frozen corpus.
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
BIN=${1:?usage: gen-masks.sh BIN}
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
BINDIR=$(cd "$(dirname "$BIN")" && pwd)
GEN=$BINDIR/gen
CHECK=$BINDIR/check
ARENA=$BOX/arena
OUT=$BOX/out
RAWROOT=$ARENA/raw
mkdir -p "$OUT" "$RAWROOT" "$ARENA/golden" "$ARENA/hidden" "$ARENA/runs"
[ -e "$RAWROOT/raw" ] || ln -sfn "$BOX/raw" "$RAWROOT/raw"
cp -f "$BINDIR/seed" "$ARENA/check/seed" 2>/dev/null || true
export GOMAXPROCS=16

# wrapper for check's -bin: same env as the timed runs.
WRAPPER=${CHECK_WRAPPER:-$BINDIR/wrapper-$(basename "$BIN")}
if [ ! -x "$WRAPPER" ]; then
	WRAPPER=$OUT/wrapper-self
	{
		echo '#!'/bin/sh
		echo 'export GOMAXPROCS=16'
		echo "exec \"$BIN\" \"\$@\""
	} > "$WRAPPER"
	chmod +x "$WRAPPER"
fi

SG=${SEED_GOLDEN:-1}
SH=${SEED_HIDDEN:-2}

echo "== gen golden (seed $SG) from $BOX/cold-3"
"$GEN" -root "$BOX/cold-3" -oracle "$BIN" -seed "$SG" -out "$ARENA/golden" 2>&1 | tee "$OUT/gen-golden.log"
echo "== gen hidden (seed $SH) from $BOX/cold-3"
"$GEN" -root "$BOX/cold-3" -oracle "$BIN" -seed "$SH" -out "$ARENA/hidden" 2>&1 | tee "$OUT/gen-hidden.log"
wc -l "$ARENA/golden/calls.jsonl" "$ARENA/hidden/calls.jsonl" "$ARENA/golden/expected.jsonl" "$ARENA/hidden/expected.jsonl"

echo "== masks: cold-3 answers vs cold-1 answers"
for set in golden hidden; do
	"$BIN" answer -root "$BOX/cold-3" < "$ARENA/$set/calls.jsonl" > "$OUT/masks-$set-e1.jsonl"
	"$BIN" answer -root "$BOX/cold-1" < "$ARENA/$set/calls.jsonl" > "$OUT/masks-$set-e2.jsonl"
	"$CHECK" masks -set "$set" -a "$ARENA/$set/expected.jsonl" -b "$OUT/masks-$set-e2.jsonl" -out "$ARENA/check/masks.json"
done
wc -c "$ARENA/check/masks.json"

if [ "${SKIP_CHECK:-0}" = "1" ]; then
	echo "SKIP_CHECK=1: self-check skipped"
	exit 0
fi

echo "== self-check (clean phase only; one more full ingest)"
set +e
"$CHECK" -bin "$WRAPPER" -name oracle-self -arena "$ARENA" \
	-skip incremental,crash,appendcrash 2>&1 | tee "$OUT/oracle-self.log"
# check exits nonzero when the scorecard says fail; the scorecard file is truth.
set -e
SCORE=$(ls -t "$ARENA"/runs/oracle-self-*.json | head -1)
python3 -I - "$SCORE" > "$OUT/oracle-self-summary.txt" <<'PYEOF'
import json, sys
c = json.load(open(sys.argv[1]))
print("pass", c.get("pass"))
for tool, t in sorted((c.get("tools") or {}).items()):
    print("%s %d/%d" % (tool, t.get("pass", 0), t.get("pass", 0) + t.get("fail", 0)))
ing = c.get("clean_ingest") or {}
print("clean_ingest_wall_ms", ing.get("WallMs"), "peak_rss_mib",
      round((ing.get("PeakRSS") or 0) / 1024.0, 1))
PYEOF
cat "$OUT/oracle-self-summary.txt"
exit "$(grep -q '^pass True' "$OUT/oracle-self-summary.txt" && echo 0 || echo 1)"
