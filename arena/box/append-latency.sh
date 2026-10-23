#!/bin/sh
# append-latency.sh — append-to-searchable latency for a 10-row append.
#
## Inputs
##   $1  BIN  the binary under test (`ingest`/`batch`, arena contract).
## Requires: ingest-timed.sh ran first for this BIN — BASE_STORE (default
##           $BOX/cold-1) is the pre-built store the append lands on.
## Env: BOX (default /var/lib/mnemonic-e3). RUNS (default 5). SETUP=1 forces
##      the deterministic target/donor/term choice to be redone; setup also
##      redoes itself whenever BIN differs from the one recorded in the state.
#
# Scratch state lives on the box under $BOX/append/ and $BOX/out/append-* (the
# copied lines, the chosen term and session — identifiers only, never printed):
# - target and donor are two different sources of the FIRST author=* dir (the
#   author the contract's batch acts as), same harness= dir, both >= 1 MiB;
#   picked deterministically from the sorted manifest list.
# - 10 whole plain lines (lines 2-11) are decompressed from the donor with the
#   zstd CLI; the search term is the longest [a-z][a-z0-9_]{5,} token among
#   them that a search_sessions call on the pristine store returns in its top
#   hit; that top hit's session id is what must stay in the reply.
# - the pre-append reply of the timed call is saved against the base store.
#
# Each of RUNS rounds then does, timed only where marked:
#   1. fresh overlay dataroot: cp -al of the frozen raw; the target's
#      .jsonl.zst and .json hardlinks are replaced by real writable copies —
#      the frozen corpus itself is never written;
#   2. the 10 lines are re-compressed as ONE NEW zstd frame (zstd CLI) and
#      appended to the target's .jsonl.zst; its manifest is updated:
#      zst_size = new file size, size += appended plain bytes, raw_end = size,
#      updated_ms = now ms (head_hash untouched: first line unchanged);
#   3. fresh run store: a real cp -a of the base store (a hardlink clone would
#      let the converge write through into the base store);
#   4. timed: `BIN ingest -root overlay -store runstore` under /usr/bin/time
#      -v with 1 s vmstat (and pidstat when installed) alongside;
#   5. timed: `BIN batch` with ONE search_sessions call (query = term,
#      n_hits = 10); correctness = the pre-append top-hit session is still in
#      the reply, and the reply's data must differ from the pre-append reply
#      (evidence the append reached the index);
#   6. overlay and run store deleted; one TSV line appended.
# Summary: $BOX/out/append-summary.tsv (run ingest_wall_s ingest_rss_mib
# batch_wall_s call_us session_found hits differs) plus a median/range footer.
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
RUNS=${RUNS:-5}
BIN=${1:?usage: append-latency.sh BIN}
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
OUT=$BOX/out
APP=$BOX/append
BASE_STORE=${BASE_STORE:-$BOX/cold-1}
export GOMAXPROCS=16
unset GOMEMLIMIT || true
mkdir -p "$OUT"

state_fresh() {
	[ -s "$OUT/append-state.env" ] || return 1
	[ "$(grep '^bin=' "$OUT/append-state.env" | cut -d= -f2-)" = "$BIN" ] || return 1
	return 0
}

# ---- setup: pick target, donor, term, session; save the pre-append reply ----
if [ "${SETUP:-0}" = "1" ] || ! state_fresh; then
	rm -rf "$APP"
	mkdir -p "$APP"
	AUTHOR=$(cd "$BOX/raw" && ls | grep '^author=' | sort | head -1)
	HARNESS=$(cd "$BOX/raw/$AUTHOR" && for d in */harness=*; do
		echo "$(find "$d" -name '*.json' | wc -l) $d"; done | sort -rn | head -1 | cut -d' ' -f2-)
	echo "append setup: sources=$(find "$BOX/raw/$AUTHOR/$HARNESS" -name '*.json' | wc -l)"
	PICK=$(python3 -I - "$BOX/raw/$AUTHOR/$HARNESS" <<'PYEOF'
import json, os, sys
root = sys.argv[1]
cands = []
for name in sorted(os.listdir(root)):
    if not name.endswith(".json"):
        continue
    man = os.path.join(root, name)
    zst = man[:-5] + ".jsonl.zst"
    try:
        m = json.load(open(man))
        if int(m.get("size", 0)) >= 1 << 20 and os.path.getsize(zst) == int(m["zst_size"]):
            cands.append((man, zst))
    except Exception:
        continue
if len(cands) < 4:
    sys.exit("too few big sources")
print(cands[len(cands) // 3][0])
print(cands[len(cands) // 3][1])
print(cands[2 * len(cands) // 3][1])
PYEOF
)
	TGT_MAN=$(echo "$PICK" | sed -n 1p)
	TGT_ZST=$(echo "$PICK" | sed -n 2p)
	DON_ZST=$(echo "$PICK" | sed -n 3p)
	zstd -q -dc "$DON_ZST" | sed -n '2,11p' > "$APP/lines.jsonl"
	[ "$(wc -l < "$APP/lines.jsonl")" -eq 10 ] || { echo "donor gave fewer than 10 lines" >&2; exit 1; }
	python3 -I - "$APP/lines.jsonl" > "$APP/terms.txt" <<'PYEOF'
import re, sys
t = open(sys.argv[1], encoding="utf-8", errors="replace").read()
toks = sorted(set(re.findall(r"[a-z][a-z0-9_]{5,}", t)), key=lambda s: (-len(s), s))
for s in toks[:10]:
    print(s)
PYEOF
	# validate terms against the pristine store until one returns a hit
	TERM=""; SESSION=""
	while IFS= read -r cand; do
		printf '{"id":1,"tool":"search_sessions","args":{"query":"%s","n_hits":10}}\n' "$cand" \
			| "$BIN" batch -root "$BOX" -store "$BASE_STORE" > "$APP/term-reply.jsonl" 2>/dev/null \
			|| continue
		GOT=$(python3 -I - "$APP/term-reply.jsonl" <<'PYEOF'
import json, sys
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r.get("error"):
        continue
    for res in ((r.get("data") or {}).get("results") or []):
        o = res.get("open") or {}
        if o.get("session_id"):
            print(o["session_id"])
            raise SystemExit
PYEOF
)
		if [ -n "$GOT" ]; then TERM=$cand; SESSION=$GOT; break; fi
	done < "$APP/terms.txt"
	[ -n "$TERM" ] || { echo "no candidate term matched the pristine store" >&2; exit 1; }
	REL_MAN=${TGT_MAN#"$BOX/raw/"}
	{
		echo "bin=$BIN"
		echo "target_rel=$REL_MAN"
		echo "term=$TERM"
		echo "session=$SESSION"
	} > "$OUT/append-state.env"
	cp -f "$APP/term-reply.jsonl" "$OUT/append-prereply.jsonl"
	printf '{"id":1,"tool":"search_sessions","args":{"query":"%s","n_hits":10}}\n' "$TERM" \
		> "$OUT/append-call.jsonl"
	echo "append setup: state rewritten for this bin"
fi
. "$OUT/append-state.env"
TGT_ZST=$BOX/raw/${target_rel%.json}.jsonl.zst
TGT_MAN=$BOX/raw/$target_rel
ADDSZ=$(wc -c < "$APP/lines.jsonl")

: > "$OUT/append-summary.tsv"
for i in $(seq 1 "$RUNS"); do
	OVL=$APP/overlay
	RST=$APP/store
	rm -rf "$OVL" "$RST"
	mkdir -p "$OVL"
	cp -al "$BOX/raw" "$OVL/raw"
	OTGT=$OVL/raw/${target_rel%.json}.jsonl.zst
	OMAN=$OVL/raw/$target_rel
	# break the hardlinks of the two files this round rewrites
	cp --remove-destination "$TGT_ZST" "$OTGT"
	chmod u+w "$OTGT"
	cp --remove-destination "$TGT_MAN" "$OMAN"
	chmod u+w "$OMAN"
	zstd -q -3 -f "$APP/lines.jsonl" -o "$APP/frame.zst"
	cat "$APP/frame.zst" >> "$OTGT"
	python3 -I - "$OMAN" "$OTGT" "$ADDSZ" <<'PYEOF'
import json, os, sys, time
man, zst, add = sys.argv[1], sys.argv[2], int(sys.argv[3])
m = json.load(open(man))
m["size"] = int(m.get("size", 0)) + add
m["raw_end"] = m["size"]
m["zst_size"] = os.path.getsize(zst)
m["updated_ms"] = int(time.time() * 1000)
json.dump(m, open(man, "w"))
PYEOF
	cp -a "$BASE_STORE" "$RST"
	LOG=$OUT/append-$i.log
	: > "$LOG"
	if command -v pidstat >/dev/null 2>&1; then
		pidstat -u -d -h 1 2>> "$LOG" &
		PIDS=$!
	fi
	vmstat 1 >> "$LOG" 2>&1 &
	VMS=$!
	set +e
	/usr/bin/time -v "$BIN" ingest -root "$OVL" -store "$RST" >> "$LOG" 2>&1
	IRC=$?
	/usr/bin/time -f 'batch_stats %e %M' "$BIN" batch -root "$OVL" -store "$RST" \
		< "$OUT/append-call.jsonl" > "$APP/reply-$i.jsonl" 2>> "$LOG"
	BRC=$?
	set -e
	kill "$VMS" 2>/dev/null || true
	[ -z "${PIDS:-}" ] || kill "$PIDS" 2>/dev/null || true
	wait "$VMS" 2>/dev/null || true
	[ -z "${PIDS:-}" ] || wait "$PIDS" 2>/dev/null || true
	if [ "$IRC" -ne 0 ] || [ "$BRC" -ne 0 ]; then
		echo "append run $i: ingest=$IRC batch=$BRC, see $LOG" >&2
		exit 1
	fi
	python3 -I - "$i" "$LOG" "$APP/reply-$i.jsonl" "$OUT/append-prereply.jsonl" "$SESSION" \
		>> "$OUT/append-summary.tsv" <<'PYEOF'
import json, re, sys
run, log, reply, pre, session = sys.argv[1:6]
t = open(log, encoding="utf-8", errors="replace").read()
m = re.search(r"Elapsed \(wall clock\) time [^:]*: (\S+)", t)
secs = 0.0
for p in (m.group(1) if m else "").split(":"):
    secs = secs * 60 + float(p or 0)
m = re.search(r"Maximum resident set size \(kbytes\): (\d+)", t)
rss = int(m.group(1)) / 1024 if m else 0.0
m = re.search(r"batch_stats ([\d.]+) (\d+)", t)
bwall = float(m.group(1)) if m else 0.0
r = [json.loads(l) for l in open(reply)]
p = [json.loads(l) for l in open(pre)]
found, hits = 0, 0
for x in r:
    for res in ((x.get("data") or {}).get("results") or []):
        o = res.get("open") or {}
        if o.get("session_id"):
            hits += 1
            if o["session_id"] == session:
                found = 1
same = (json.dumps(r[0].get("data"), sort_keys=True) ==
        json.dumps(p[0].get("data"), sort_keys=True)) if r and p else True
print("\t".join([run, "%.1f" % secs, "%.1f" % rss, "%.2f" % bwall,
                 str(r[0].get("us", 0) if r else 0),
                 str(found), str(hits), str(int(not same))]))
PYEOF
	rm -rf "$OVL" "$RST"
done

python3 -I - "$OUT/append-summary.tsv" <<'PYEOF'
import sys
rows = [l.split("\t") for l in open(sys.argv[1]).read().splitlines() if l.strip()]
ing = sorted(float(r[1]) for r in rows)
call = sorted(int(r[4]) for r in rows)
med = lambda xs: xs[len(xs) // 2] if len(xs) % 2 else (xs[len(xs)//2 - 1] + xs[len(xs)//2]) / 2
print("median_ingest_wall_s\t%.1f" % med(ing))
print("range_ingest_wall_s\t%.1f-%.1f" % (ing[0], ing[-1]))
print("median_call_us\t%d" % med(call))
print("range_call_us\t%d-%d" % (call[0], call[-1]))
print("session_found_all_runs\t%s" % all(r[5] == "1" for r in rows))
print("reply_differs_all_runs\t%s" % all(r[7] == "1" for r in rows))
PYEOF
