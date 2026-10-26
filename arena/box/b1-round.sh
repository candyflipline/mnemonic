#!/bin/sh
# b1-round.sh — lane B1: one timed append round (H, C or M) on fresh overlayfs
# views of the frozen raw and of a settled base store (the frozen dirs are
# lowerdirs, never written; each round's writes land in its own upper dir).
# The timed driver runs at GOMAXPROCS=4 under MemoryMax=8G (prod's shape),
# then one marker search must find the appended session. For M (mixed
# raw+archive) the root also holds the 16 ch-* hardlinks and the round
# reports whether any archive member .light/.heavy log reappeared in the
# store's upper dir (it must not).
#   b1-round.sh CASE OUTD [BASE]  (BASE defaults: H,C -> b1/store-r; M -> b1/store-ra)
# Run under the box lock. Counts and timings only; never prints corpus
# content. Nothing is deleted: each round leaves its upper dirs.
set -eu
BOX=/var/lib/mnemonic-e3
P=/root/perf
OUT=${OUT:-$BOX/out/b1}
CASE=$1
OUTD=$2
case "$CASE" in
H | C) BASE=${3:-$BOX/b1/store-r} ;;
M) BASE=${3:-$BOX/b1/store-ra} ;;
*) echo "usage: b1-round.sh H|C|M OUTD [BASE]" >&2; exit 1 ;;
esac
DRV=${DRV:?driver test binary}
CLI=${CLI:?mnemonic binary for the marker search}
GMP=${GMP:-4}
[ -f "$P/$CASE-target" ] && [ -f "$P/$CASE-sid" ] && [ -f "$P/$CASE-lines.jsonl" ] || {
	echo "b1-round.sh: $P/$CASE target files missing (run b1-mpick.sh for M)" >&2
	exit 1
}
free=$(df --output=avail -B1 / | tail -1 | tr -d ' ')
[ "${free:-0}" -ge $((10 * 1024 * 1024 * 1024)) ] || { echo "DISK GUARD: free ${free}B < 10GiB, refusing" >&2; exit 2; }

mkdir -p "$OUTD"
N=$(date +%s%N)
R=$BOX/b1/r-$CASE-$N
mkdir -p "$R/rawup" "$R/rawwork" "$R/ovl/raw" "$R/stup" "$R/stwork" "$R/store"
mount -t overlay overlay -o "lowerdir=$BOX/raw,upperdir=$R/rawup,workdir=$R/rawwork" "$R/ovl/raw"
mount -t overlay overlay -o "lowerdir=$BASE,upperdir=$R/stup,workdir=$R/stwork" "$R/store"
if [ "$CASE" = M ]; then
	while IFS= read -r p; do
		rel=${p#"$BOX/archive/"}
		mkdir -p "$R/ovl/$(dirname "$rel")"
		ln "$p" "$R/ovl/$rel"
	done < "$OUT/ch-list.txt"
fi
OVL=$R/ovl
RST=$R/store
rel=$(cat "$P/$CASE-target")
OTGT=$OVL/raw/${rel%.json}.jsonl.zst
OMAN=$OVL/raw/$rel
zstd -q -3 -f "$P/$CASE-lines.jsonl" -o "$R/frame.zst"
cat "$R/frame.zst" >> "$OTGT"
python3 -I - "$OMAN" "$OTGT" "$(wc -c < "$P/$CASE-lines.jsonl")" <<'PY'
import json, os, sys, time
man, zst, add = sys.argv[1], sys.argv[2], int(sys.argv[3])
m = json.load(open(man))
m["size"] = int(m.get("size", 0)) + add
m["raw_end"] = m["size"]
m["zst_size"] = os.path.getsize(zst)
m["updated_ms"] = int(time.time() * 1000)
json.dump(m, open(man, "w"))
PY
sync
LOG=$OUTD/$CASE-$N.log
: > "$LOG"
pidstat -u -h 1 > "${LOG%.log}.pidstat" 2>&1 &
PS=$!
set +e
/usr/bin/time.gnu -v systemd-run --scope --unit=b1-$CASE-$N -p MemoryMax=8G \
	env GOMAXPROCS=$GMP PERF_ROOT="$OVL" PERF_STORE="$RST" \
	"$DRV" -test.run '^TestPerfCycle$' -test.v -test.count=1 >> "$LOG" 2>&1
RC=$?
set -e
kill "$PS" 2>/dev/null || true
printf '{"id":1,"tool":"search_sessions","args":{"query":"qz9u9cappendprobe","n_hits":10}}\n' \
	| GOMAXPROCS=4 "$CLI" index batch -root "$OVL" -store "$RST" > "${LOG%.log}.reply" 2>/dev/null
python3 -I - "${LOG%.log}.reply" "$(cat "$P/$CASE-sid")" >> "$LOG" <<'PY'
import base64, json, sys
r = json.loads(open(sys.argv[1]).readline()); t = sys.argv[2]
ids = [(x.get("open") or {}).get("session_id") for x in ((r.get("data") or {}).get("results") or [])]
tf = {t, "session:" + base64.b64encode(t.encode()).decode(), "session:" + base64.urlsafe_b64encode(t.encode()).decode().rstrip("=")}
print("correct target_found=%d hits=%d" % (int(any(i in tf for i in ids)), len(ids)))
PY
echo "rc=$RC" >> "$LOG"
if [ "$CASE" = M ]; then
	TGTKEY=$(sed 's#^author=\(.*\)/machine=\(.*\)/harness=\(.*\)/\(.*\)\.json$#\1__\2__\3__\4#' "$P/$CASE-target")
	{
		echo "upper_log_files_total $(find "$R/stup" -name '*.light' -o -name '*.heavy' | wc -l)"
		echo "upper_log_files_not_target $(find "$R/stup" -name '*.light' -o -name '*.heavy' | grep -v -F -- "$TGTKEY" | wc -l)"
		find "$R/stup" -name '*.light' -o -name '*.heavy' | grep -v -F -- "$TGTKEY" | sed 's#.*/src/##' > "${LOG%.log}.reappeared"
	} >> "$LOG"
fi
umount "$R/store"
umount "$OVL/raw"
echo "upper_bytes $(du -sb "$R" | cut -f1)" >> "$LOG"
echo "log $LOG"
grep -E '^perf|^=== RUN|Maximum resident|Elapsed .wall|User time|System time|Percent of CPU|correct|rc=|upper_|DISK' "$LOG" | head -30
