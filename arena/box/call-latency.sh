#!/bin/sh
# call-latency.sh — per-call latency of search_sessions and file_attention.
#
## Inputs
##   $1  BIN  the binary under test (`batch`, arena contract).
## Requires: gen-masks.sh produced $BOX/arena/golden/calls.jsonl and
##           $BOX/arena/hidden/calls.jsonl, and ingest-timed.sh left
##           $BOX/cold-3 (the store the calls run against).
## Env: BOX (default /var/lib/mnemonic-e3). RUNS (default 5).
#
# RUNS fresh `BIN batch -root $BOX -store $BOX/cold-3` processes (GOMAXPROCS=16),
# each fed the golden calls then the hidden calls in one stdin stream (one
# process, both sets, the order check uses); replies to $BOX/out/lat-<i>.jsonl.
# Per run, per tool over BOTH sets combined: n, p50 and p95 of the reply's `us`
# field (same nearest-rank percentiles as arena/check), errors = replies with a
# non-empty error. Footer: median of the p50s per tool and the range across
# runs. Summary: $BOX/out/call-latency-summary.tsv.
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
RUNS=${RUNS:-5}
BIN=${1:?usage: call-latency.sh BIN}
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
OUT=$BOX/out
CALLS=$BOX/arena
export GOMAXPROCS=16
mkdir -p "$OUT"

: > "$OUT/call-latency-summary.tsv"
cat "$CALLS/golden/calls.jsonl" "$CALLS/hidden/calls.jsonl" > "$OUT/all-calls.jsonl"
for i in $(seq 1 "$RUNS"); do
	set +e
	/usr/bin/time -f 'batch_wall_s %e' "$BIN" batch -root "$BOX" -store "$BOX/cold-3" \
		< "$OUT/all-calls.jsonl" \
		> "$OUT/lat-$i.jsonl" 2> "$OUT/lat-$i.log"
	RC=$?
	set -e
	[ "$RC" -eq 0 ] || { echo "batch run $i exited $RC, see $OUT/lat-$i.log" >&2; exit "$RC"; }
	python3 -I - "$i" "$OUT/lat-$i.jsonl" "$OUT/all-calls.jsonl" >> "$OUT/call-latency-summary.tsv" <<'PYEOF'
import json, math, sys
run, rpath, cpath = sys.argv[1], sys.argv[2], sys.argv[3]
calls = [json.loads(l) for l in open(cpath)]
replies = [json.loads(l) for l in open(rpath)]
if len(calls) != len(replies):
    sys.exit("%d replies for %d calls" % (len(replies), len(calls)))
by = {}
for c, r in zip(calls, replies):
    if r.get("id") != c["id"]:
        sys.exit("reply id %s for call id %s" % (r.get("id"), c["id"]))
    by.setdefault(c["tool"], []).append(r)
for tool in ("search_sessions", "file_attention"):
    us = sorted(x.get("us", 0) for x in by.get(tool, []))
    errs = sum(1 for x in by.get(tool, []) if x.get("error"))
    if not us:
        print("\t".join([run, tool, "0", "0", "0", str(errs)]))
        continue
    q = lambda f: us[min(len(us) - 1, int(math.ceil(f * len(us))) - 1)]
    print("\t".join([run, tool, str(len(us)), str(q(0.5)), str(q(0.95)), str(errs)]))
PYEOF
done

python3 -I - "$OUT/call-latency-summary.tsv" <<'PYEOF'
import sys
rows = [l.split("\t") for l in open(sys.argv[1]).read().splitlines() if l.strip()]
med = lambda xs: xs[len(xs) // 2] if len(xs) % 2 else (xs[len(xs)//2 - 1] + xs[len(xs)//2]) / 2
for tool in ("search_sessions", "file_attention"):
    p50s = sorted(int(r[3]) for r in rows if r[1] == tool)
    p95s = sorted(int(r[4]) for r in rows if r[1] == tool)
    errs = sum(int(r[5]) for r in rows if r[1] == tool)
    if not p50s:
        continue
    print("median_p50_us\t%s\t%d" % (tool, med(p50s)))
    print("range_p50_us\t%s\t%d-%d" % (tool, p50s[0], p50s[-1]))
    print("median_p95_us\t%s\t%d" % (tool, med(p95s)))
    print("errors_all_runs\t%s\t%d" % (tool, errs))
PYEOF
