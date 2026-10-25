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
# - the target session id is the target source's own sessionId (its first
#   user/assistant line). 10 whole user/assistant lines are decompressed from
#   the donor with the zstd CLI and their sessionId REWRITTEN to the target's:
#   appended as-is they would only land on the donor session (already
#   searchable — EventUID hashes the source file, so the copies are new rows
#   of whatever session their sessionId names). A fixed nonce marker
#   (qz9u9cappendprobe) is appended to the first message-text item of the
#   copied lines: mined terms cannot guarantee rarity, and a broad term's
#   posted docs crowd the appended copy out of the search's top n+1
#   candidate docs no matter how correctly it is indexed (two real rounds
#   proved both halves). The marker matches nothing pre-append — proven by
#   one batch against the base store — so post-append ONLY the appended rows
#   can match, which is exactly the packet's correctness predicate: the
#   post-append reply contains the target session, which the pre-append
#   reply lacked.
# - the pre-append reply of the timed call is saved against the base store.
#
# Each of RUNS rounds then does, timed only where marked:
#   1. fresh overlay dataroot: cp -al of the frozen raw; the target's
#      .jsonl.zst and .json hardlinks are replaced by real writable copies —
#      the frozen corpus itself is never written;
#   2. the 10 rewritten lines are re-compressed as ONE NEW zstd frame (zstd
#      CLI) and appended to the target's .jsonl.zst; its manifest is updated:
#      zst_size = new file size, size += appended plain bytes, raw_end = size,
#      updated_ms = now ms (head_hash untouched: first line unchanged);
#   3. fresh run store: a real cp -a of the base store (a hardlink clone would
#      let the converge write through into the base store);
#   4. timed: `BIN ingest -root overlay -store runstore` under /usr/bin/time
#      -v with 1 s vmstat (and pidstat when installed) alongside;
#   5. timed: `BIN batch` with ONE search_sessions call (query = term,
#      n_hits = 10); correctness = the pre-append top-hit session is still in
#      the reply (session_found), the TARGET session is in the reply but was
#      NOT in the pre-append one (target_new — the 10 rows landed and became
#      searchable under their rewritten session), and the ingest's own
#      summary line reports at least one parsed source (oracle rewrote,
#      engine resumed+reparsed). A round failing any of the three writes its
#      row, then exits nonzero: the round's numbers time a no-op, not an
#      append.
#   6. overlay and run store deleted; one TSV line appended.
# Summary: $BOX/out/append-summary.tsv (run ingest_wall_s ingest_rss_mib
# batch_wall_s call_us session_found hits new_hit new_count parsed events
# differs target_new) plus a median/range footer.
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
RUNS=${RUNS:-5}
BIN=${1:?usage: append-latency.sh BIN}
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
# OUT overridable so two lanes' runs (oracle and engine) never share the
# summary/state files; each lane passes its own dir.
OUT=${OUT:-$BOX/out}
APP=$BOX/append
BASE_STORE=${BASE_STORE:-$BOX/cold-1}
export GOMAXPROCS=16
unset GOMEMLIMIT || true
mkdir -p "$OUT"

state_fresh() {
	[ -s "$OUT/append-state.env" ] || return 1
	[ "$(grep '^bin=' "$OUT/append-state.env" | cut -d= -f2-)" = "$BIN" ] || return 1
	[ "$(grep '^base=' "$OUT/append-state.env" | cut -d= -f2-)" = "$BASE_STORE" ] || return 1
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
	# helpers: piped decompressed lines on stdin, so the python lives in files
	cat > "$APP/first_sid.py" <<'PYEOF'
import json, sys
for line in sys.stdin:
    try:
        o = json.loads(line)
    except Exception:
        continue
    if o.get("type") in ("user", "assistant") and o.get("sessionId"):
        print(o["sessionId"])
        break
PYEOF
	cat > "$APP/donor10.py" <<'PYEOF'
import json, sys
sid = sys.argv[1]
marker = sys.argv[2]
n = 0
marked = False
for line in sys.stdin:
    if n >= 10:
        break
    line = line.rstrip("\n")
    if not line:
        continue
    try:
        o = json.loads(line)
    except Exception:
        continue
    if o.get("type") not in ("user", "assistant") or not o.get("sessionId"):
        continue
    o["sessionId"] = sid
    if not marked:
        m = o.get("message")
        if isinstance(m, dict):
            c = m.get("content")
            if isinstance(c, str):
                o["message"]["content"] = c + " " + marker
                marked = True
            elif isinstance(c, list):
                for it in c:
                    if isinstance(it, dict) and it.get("type") == "text" \
                            and isinstance(it.get("text"), str):
                        it["text"] = it["text"] + " " + marker
                        marked = True
                        break
    sys.stdout.write(json.dumps(o, separators=(",", ":"), ensure_ascii=False) + "\n")
    n += 1
if n < 10:
    sys.exit("donor gave fewer than 10 user/assistant lines")
if not marked:
    sys.exit("no message-text item to carry the marker")
PYEOF
	TARGET_SID=$(zstd -q -dc "$TGT_ZST" | python3 -I "$APP/first_sid.py")
	[ -n "$TARGET_SID" ] || { echo "no target session id in the target source" >&2; exit 1; }
	MARKER=qz9u9cappendprobe
	zstd -q -dc "$DON_ZST" | python3 -I "$APP/donor10.py" "$TARGET_SID" "$MARKER" > "$APP/lines.jsonl"
	[ "$(wc -l < "$APP/lines.jsonl")" -eq 10 ] || { echo "donor gave fewer than 10 lines" >&2; exit 1; }
	# the marker must match nothing pre-append: prove it once against the
	# base store, and that reply is the pre-append reply of the timed call
	printf '{"id":1,"tool":"search_sessions","args":{"query":"%s","n_hits":10}}\n' "$MARKER" \
		| "$BIN" batch -root "$BOX" -store "$BASE_STORE" > "$OUT/append-prereply.jsonl" 2>/dev/null
	NRES=$(python3 -I - "$OUT/append-prereply.jsonl" <<'PYEOF'
import json, sys
n = 0
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r.get("error"):
        sys.exit("marker pre-check errored: %s" % r["error"][:120])
    for res in ((r.get("data") or {}).get("results") or []):
        if (res.get("open") or {}).get("session_id"):
            n += 1
print(n)
PYEOF
)
	[ "$NRES" -eq 0 ] || { echo "marker already matches $NRES sessions pre-append" >&2; exit 1; }
	REL_MAN=${TGT_MAN#"$BOX/raw/"}
	{
		echo "bin=$BIN"
		echo "base=$BASE_STORE"
		echo "target_rel=$REL_MAN"
		echo "term=$MARKER"
		echo "session="
		echo "target_sid=$TARGET_SID"
	} > "$OUT/append-state.env"
	printf '{"id":1,"tool":"search_sessions","args":{"query":"%s","n_hits":10}}\n' "$MARKER" \
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
	python3 -I - "$i" "$LOG" "$APP/reply-$i.jsonl" "$OUT/append-prereply.jsonl" "$session" "$target_sid" \
		>> "$OUT/append-summary.tsv" <<'PYEOF'
import base64, json, re, sys
run, log, reply, pre, session, target = sys.argv[1:7]
t = open(log, encoding="utf-8", errors="replace").read()
m = re.search(r"Elapsed \(wall clock\) time[^\n]*: (\S+)", t)
secs = 0.0
for p in (m.group(1) if m else "").split(":"):
    secs = secs * 60 + float(p or 0)
m = re.search(r"Maximum resident set size \(kbytes\): (\d+)", t)
rss = int(m.group(1)) / 1024 if m else 0.0
m = re.search(r"batch_stats ([\d.]+) (\d+)", t)
bwall = float(m.group(1)) if m else 0.0
# the append must have reached the index: the oracle counts rewritten
# sources, the engine resumed+reparsed ones; only the oracle prints events
m = re.search(r"ingest: sources=(\d+) rewrote=(\d+) failed=(\d+) events=(\d+)", t)
if m:
    parsed, events = int(m.group(2)), int(m.group(4))
else:
    m = re.search(r"ingest: unchanged=(\d+) resumed=(\d+) fresh=(\d+) reparsed=(\d+)", t)
    parsed = int(m.group(2)) + int(m.group(4)) if m else -1
    events = -1
def ids(line):
    out = []
    for res in ((line.get("data") or {}).get("results") or []):
        o = res.get("open") or {}
        if o.get("session_id"):
            out.append(o["session_id"])
    return out
r = [json.loads(l) for l in open(reply)]
p = [json.loads(l) for l in open(pre)]
post = ids(r[0]) if r else []
pre_ids = set(ids(p[0])) if p else set()
hits = len(post)
found = 1 if (not session or session in post) else 0
new = [s for s in post if s not in pre_ids]
same = (json.dumps(r[0].get("data"), sort_keys=True) ==
        json.dumps(p[0].get("data"), sort_keys=True)) if r and p else True
tforms = {target, "session:" + base64.b64encode(target.encode()).decode()}
tnew = any(s in tforms for s in post) and not any(s in tforms for s in pre_ids)
print("\t".join([run, "%.1f" % secs, "%.1f" % rss, "%.2f" % bwall,
                 str(r[0].get("us", 0) if r else 0),
                 str(found), str(hits), str(int(bool(new))), str(len(new)),
                 str(parsed), str(events), str(int(not same)), str(int(tnew))]))
if parsed < 1 or not tnew:
    sys.stderr.write(
        "append run %s INVALID: parsed=%d target_new=%s new_hits=%d — the "
        "append did not become searchable under the target session; see %s\n"
        % (run, parsed, bool(tforms & set(post)), len(new), log))
    raise SystemExit(3)
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
print("new_hit_all_runs\t%s" % all(r[7] == "1" for r in rows))
print("min_parsed_sources\t%d" % min(int(r[9]) for r in rows))
print("reply_differs_all_runs\t%s" % all(r[11] == "1" for r in rows))
print("target_new_all_runs\t%s" % all(r[12] == "1" for r in rows))
PYEOF
