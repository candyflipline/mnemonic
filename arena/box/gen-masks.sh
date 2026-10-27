#!/bin/sh
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
BIN=${1:?usage: gen-masks.sh BIN}
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
BINDIR=$(cd "$(dirname "$BIN")" && pwd)
GEN=$BINDIR/gen
CHECK=$BINDIR/check
ARENA=$BOX/arena
OUT=$BOX/out
GEN_ROOT=${GEN_ROOT:-$BOX/cold-1}
MASK_ROOT=${MASK_ROOT:-$BOX/cold-2}
RAWROOT=$ARENA/raw
mkdir -p "$OUT" "$RAWROOT" "$ARENA/golden" "$ARENA/hidden" "$ARENA/runs"
[ -e "$RAWROOT/raw" ] || ln -sfn "$BOX/raw" "$RAWROOT/raw"
cp -f "$BINDIR/seed" "$ARENA/check/seed" 2>/dev/null || true
export GOMAXPROCS=16

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

echo "== gen golden (seed $SG) from $GEN_ROOT"
"$GEN" -root "$GEN_ROOT" -oracle "$BIN" -seed "$SG" -out "$ARENA/golden" 2>&1 | tee "$OUT/gen-golden.log"
echo "== gen hidden (seed $SH) from $GEN_ROOT"
"$GEN" -root "$GEN_ROOT" -oracle "$BIN" -seed "$SH" -out "$ARENA/hidden" 2>&1 | tee "$OUT/gen-hidden.log"
wc -l "$ARENA/golden/calls.jsonl" "$ARENA/hidden/calls.jsonl" "$ARENA/golden/expected.jsonl" "$ARENA/hidden/expected.jsonl"

echo "== masks: $GEN_ROOT answers vs $MASK_ROOT answers"
for set in golden hidden; do
	"$BIN" answer -root "$GEN_ROOT" < "$ARENA/$set/calls.jsonl" > "$OUT/masks-$set-e1.jsonl"
	"$BIN" answer -root "$MASK_ROOT" < "$ARENA/$set/calls.jsonl" > "$OUT/masks-$set-e2.jsonl"
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
