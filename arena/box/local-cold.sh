#!/bin/sh
# local-cold.sh — local-mode cold ingest of the real corpus as plain files.
#
## What it measures
##   `mnemonic local`'s cold FIRST ingest over a plain HOME of harness-native
##   JSONL files decompressed from the frozen raw store (no raw/ in the index,
##   the local-mode contract). The local process stays resident and serves MCP
##   after the first ingest, so each run records two walls from process start:
##   to the first `index: unchanged=.. fresh=` line (first ingest done, the
##   packet's metric) and to the `listening on` line (first ingest + its settle
##   drain, the number comparable to the server path's one-shot `index
##   ingest`). Peak RSS, user/sys/cpu come from /usr/bin/time -v at TERM;
##   vmstat and pidstat sample alongside for the limiter. One search_sessions
##   call over loopback MCP after the first ingest (result_count only, never
##   the content) is the correctness check.
##
##   The first ingest redacts every line (redact.Line before parse, as push
##   does — the main session's call, cmd/mnemonic/local.go:109 +
##   internal/index/kind.go redactReader at the engine tip). The plain tree is
##   built from already-redacted raw, so redaction finds little to replace; it
##   still scans every line, which is the cost being timed.
#
## Inputs
##   $1  CMD  estimate | build | run
##   $2  BIN  the binary under test, built WITH -tags server: `local` is in
##            the server build at the engine tip (the untagged box binary of
##            the same tree answers `mnemonic init|push|serve|token` only).
##            CGO_ENABLED=0 works at this tip: internal/lake is gone.
## Env: BOX (default /var/lib/mnemonic-e3) — raw/ is the frozen corpus read
##      only; plainhome/, local-N/ and out/u9d-* are this script's writes.
##      RUNS (default 1), FIRST (default 1) — run i uses data dir $BOX/local-i
##      and refuses to start when it already exists (fresh data dir each;
##      nothing is ever deleted). RUNGATE_MB (default 21500) — a run starts
##      only if free - that leaves >= FREEFLOOR_MB (default 15360) on /.
#
# estimate decodes a 1-in-41 sample of the raw manifests to /dev/null and
# projects the plain tree's size. build decompresses every source to the
# harness-native path config.DefaultSources expects under a HOME (the root
# table below is that table's fixed path prefixes), renaming the project
# directory of a colliding source so no file is overwritten. run does the
# timed cold ingests. Counters and sizes only — never corpus content.
set -eu

CMD=${1:?usage: local-cold.sh estimate|build|run BIN}
BIN=${2:?usage: local-cold.sh estimate|build|run BIN}
BOX=${BOX:-/var/lib/mnemonic-e3}
RAW=$BOX/raw
PH=$BOX/plainhome
OUT=$BOX/out
RUNS=${RUNS:-1}
FIRST=${FIRST:-1}
RUNGATE_MB=${RUNGATE_MB:-21500}
FREEFLOOR_MB=${FREEFLOOR_MB:-15360}
LISTEN=${LISTEN:-127.0.0.1:7781}
mkdir -p "$OUT"
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }

free_mb() { df -B1G --output=avail / | tail -1 | tr -d ' ' | awk '{print $1*1024}'; }

# The fixed path prefixes of config.DefaultSources (internal/config/config.go
# at the engine tip), as they appear in a client's absolute path: the glob's
# literal part between ~ and the first wildcard.
roots_table() {
	cat <<'EOF'
claude-code	claude	.claude/projects/
claude-code	claude-glm	.claude-glm/projects/
codex	codex	.codex/sessions/
cursor	cursor	.cursor/projects/
claude-code	claude-cowork	Library/Application Support/Claude/local-agent-mode-sessions/
kiro-cli	kiro	.kiro/sessions/cli/
kimi-cli	kimi	.kimi/sessions/
qwen-code	qwen	.qwen/projects/
pi-coding-agent	pi	.pi/agent/sessions/
pi-coding-agent	omp	.omp/agent/sessions/
prime-agent	prime-agent	.prime/agent/sessions/
prime-agent	prime-agent-subagents	.prime/agent/session-artifacts/
EOF
}

case $CMD in
estimate)
	# Every 41st manifest, capped at 200: decode, compare with the manifest's
	# Size (raw holds exactly the redacted bytes push received), project.
	find "$RAW" -name '*.json' | sort | awk 'NR % 41 == 1' | head -200 > "$OUT/u9d-sample-manifests.txt"
	roots_table > "$OUT/u9d-roots.tsv"
	python3 -I - "$RAW" "$OUT/u9d-sample-manifests.txt" "$OUT/u9d-roots.tsv" "$OUT/u9d-sample-decode.tsv" <<'PYEOF'
import json, subprocess, sys, os
raw, listpath, rootspath, outpath = sys.argv[1:5]
roots = {}
for line in open(rootspath):
    h, s, r = line.rstrip("\n").split("\t")
    roots[(h, s)] = r
n = ok = mismatch = unmapped = 0
total = 0
per_harness = {}
with open(outpath, "w") as out:
    for mp in open(listpath).read().splitlines():
        if not mp:
            continue
        m = json.load(open(mp))
        zst = mp[:-5] + ".jsonl.zst"
        n += 1
        h = m.get("harness", "")
        per_harness[h] = per_harness.get(h, 0) + 1
        root = roots.get((h, m.get("source_name", "")))
        if root is None or ("/" + root) not in m.get("path", ""):
            unmapped += 1
            continue
        p = subprocess.run(["zstd", "-d", "-c", zst], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if p.returncode != 0:
            print("zstd failed under", mp, file=sys.stderr)
            sys.exit(1)
        size = len(p.stdout)
        total += size
        if size == int(m.get("size", -1)):
            ok += 1
        else:
            mismatch += 1
    print("sample_n\t%d" % n, file=out)
    print("sample_decoded_bytes\t%d" % total, file=out)
    print("size_match\t%d" % ok, file=out)
    print("size_mismatch\t%d" % mismatch, file=out)
    print("unmapped\t%d" % unmapped, file=out)
    for h in sorted(per_harness):
        print("harness\t%s\t%d" % (h, per_harness[h]), file=out)
PYEOF
	free=$(free_mb)
	proj=$(awk -v t="$(grep sample_decoded_bytes "$OUT/u9d-sample-decode.tsv" | cut -f2)" -v n="$(grep sample_n "$OUT/u9d-sample-decode.tsv" | cut -f2)" 'BEGIN{printf "%.1f", t/n*8362/1073741824}')
	echo "projected_tree_gb	$proj"
	echo "free_gb	$(awk -v f=$free 'BEGIN{printf "%.1f", f/1024}')"
	awk -v p="$proj" -v f="$free" -v gate="$RUNGATE_MB" -v floor="$FREEFLOOR_MB" 'BEGIN{
		need = p*1024 + gate + floor
		print (f >= need ? "BUILD-GATE-PASS" : "BUILD-GATE-FAIL") "\tfree_mb=" f "\tneed_mb=" int(need) }'
	;;

build)
	[ -e "$PH" ] && { echo "plainhome exists: $PH (build once; nothing deletes)" >&2; exit 1; }
	roots_table > "$OUT/u9d-roots.tsv"
	python3 -I - "$RAW" "$PH" "$OUT/u9d-roots.tsv" "$OUT/u9d-plainhome-stats.txt" <<'PYEOF'
import json, os, subprocess, sys
from concurrent.futures import ThreadPoolExecutor
raw, ph, rootspath, statpath = sys.argv[1:5]
roots = {}
for line in open(rootspath):
    h, s, r = line.rstrip("\n").split("\t")
    roots[(h, s)] = r
mans = []
for dirpath, dirnames, filenames in os.walk(raw):
    for fn in filenames:
        if fn.endswith(".json"):
            mans.append(os.path.join(dirpath, fn))
mans.sort()
claimed = set()
coll = unmapped = 0
bytes_total = 0
per_harness = {}
jobs = []  # (zst, target)

def target_for(m):
    global coll, unmapped
    h = m.get("harness", "")
    root = roots.get((h, m.get("source_name", "")))
    path = m.get("path", "")
    if root is None or ("/" + root) not in path:
        unmapped += 1
        return None
    suffix = path[path.index("/" + root) + 1:]
    t = os.path.join(ph, suffix)
    if t in claimed or os.path.exists(t):
        # A distinct author/machine mapped to the same native path: give it a
        # distinct project directory under the same glob root, so the glob
        # still matches and no file is overwritten.
        coll += 1
        n = coll
        while True:
            t = os.path.join(ph, root, "u9d-%d" % n, suffix[len(root):])
            if t not in claimed and not os.path.exists(t):
                break
            n += 1
    claimed.add(t)
    return t

for mp in mans:
    m = json.load(open(mp))
    h = m.get("harness", "")
    per_harness[h] = per_harness.get(h, 0) + 1
    t = target_for(m)
    if t:
        jobs.append((mp[:-5] + ".jsonl.zst", t))

def decode(job):
    zst, t = job
    os.makedirs(os.path.dirname(t), 0o755, exist_ok=True)
    with open(zst, "rb") as f, open(t, "wb") as out:
        p = subprocess.Popen(["zstd", "-d", "-c"], stdin=f, stdout=out, stderr=subprocess.DEVNULL)
        if p.wait() != 0:
            return 1
    return 0

with ThreadPoolExecutor(max_workers=8) as ex:
    bad = sum(ex.map(decode, jobs))
if bad:
    print("zstd decode failures: %d" % bad, file=sys.stderr)
    sys.exit(1)
for zst, t in jobs:
    bytes_total += os.path.getsize(t)
with open(statpath, "w") as out:
    print("sources\t%d" % len(mans), file=out)
    print("files_written\t%d" % len(jobs), file=out)
    print("collisions\t%d" % coll, file=out)
    print("unmapped\t%d" % unmapped, file=out)
    print("bytes\t%d" % bytes_total, file=out)
    for h in sorted(per_harness):
        print("harness\t%s\t%d" % (h, per_harness[h]), file=out)
print(open(statpath).read(), end="")
PYEOF
	du -sb "$PH"
	;;

run)
	[ -d "$PH" ] || { echo "no plainhome: run build first" >&2; exit 1; }
	: > "$OUT/local-cold-summary.tsv"
	export GOMAXPROCS=16
	unset GOMEMLIMIT || true
	for i in $(seq "$FIRST" "$((FIRST + RUNS - 1))"); do
		DD=$BOX/local-$i
		if [ -e "$DD" ]; then echo "run $i: $DD exists, skipped (fresh data dir each, no deletes)" >&2; continue; fi
		free=$(free_mb)
		[ "$free" -ge $((RUNGATE_MB + FREEFLOOR_MB)) ] || {
			echo "run $i: free ${free}MB would drop under ${FREEFLOOR_MB}MB after a ${RUNGATE_MB}MB data dir, not run" >&2
			continue
		}
		LOG=$OUT/u9d-local-cold-$i.log
		: > "$LOG"
		if command -v pidstat >/dev/null 2>&1; then
			pidstat -u -d -h 1 >> "$LOG" 2>&1 &
			PIDS=$!
		fi
		vmstat 1 >> "$LOG" 2>&1 &
		VMS=$!
		mkdir -p "$DD"
		sync
		echo 3 > /proc/sys/vm/drop_caches
		START=$(date +%s.%N)
		set +e
		/usr/bin/time -v env HOME="$PH" "$BIN" local -data-dir "$DD" -listen "$LISTEN" >> "$LOG" 2>&1 &
		TPID=$!
		set -e
		# Poll for the first ingest's line, then the listening line (it comes
		# after the settle drain), then one search, then TERM; 1 s poll
		# granularity is the timing's resolution.
		while ! grep -q 'index: unchanged=' "$LOG" 2>/dev/null; do
			sleep 1
			kill -0 "$TPID" 2>/dev/null || break
		done
		T_INGEST=$(date +%s.%N)
		grep -m1 'index: unchanged=' "$LOG" >> "$OUT/local-cold-summary.lines.txt" || true
		while ! grep -q 'listening on' "$LOG" 2>/dev/null; do
			sleep 1
			kill -0 "$TPID" 2>/dev/null || break
		done
		T_LISTEN=$(date +%s.%N)
		HITS=$(curl -s --max-time 120 -X POST "http://$LISTEN/mcp" \
			-H 'Authorization: Bearer local' -H 'Mcp-Protocol-Version: 2025-06-18' \
			-H 'Content-Type: application/json' -H 'Accept: application/json' \
			-d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"error","n_hits":10}}}' \
			| python3 -I -c 'import json,sys
try:
    r = json.load(sys.stdin)
    res = r.get("result") or {}
    sc = res.get("structuredContent") or {}
    data = sc.get("data") if isinstance(sc, dict) else None
    if res.get("isError") or not isinstance(data, dict) or "result_count" not in data:
        print("no-result_count")
    else:
        print(data["result_count"])
except Exception:
    print("reply-error")') || HITS=reply-error
		LPID=$(pgrep -P "$TPID" | head -1)
		[ -n "${LPID:-}" ] && kill -TERM "$LPID" 2>/dev/null || true
		for _ in $(seq 1 60); do
			kill -0 "$TPID" 2>/dev/null || break
			sleep 1
		done
		kill "$VMS" 2>/dev/null || true
		[ -z "${PIDS:-}" ] || kill "$PIDS" 2>/dev/null || true
		wait "$VMS" 2>/dev/null || true
		[ -z "${PIDS:-}" ] || wait "$PIDS" 2>/dev/null || true
		DU=$(du -sb "$DD" | cut -f1)
		python3 -I - "$i" "$LOG" "$START" "$T_INGEST" "$T_LISTEN" "$HITS" "$DU" >> "$OUT/local-cold-summary.tsv" <<'PYEOF'
import re, sys
run, path, start, t_ing, t_lis, hits, du = sys.argv[1:8]
t = open(path, encoding="utf-8", errors="replace").read()
wall = lambda a, b: "%.1f" % (float(b) - float(a))
m = re.search(r"Maximum resident set size \(kbytes\): (\d+)", t)
rss = int(m.group(1)) / 1024 if m else 0.0
def g(pat, d=""):
    m = re.search(pat, t)
    return m.group(1) if m else d
m = re.search(r"index: unchanged=(\d+) resumed=(\d+) fresh=(\d+) reparsed=(\d+)", t)
unc, res, fresh, rep = (m.groups() if m else ("", "", "", ""))
print("\t".join([run, wall(start, t_ing), wall(start, t_lis), "%.1f" % rss,
                 g(r"User time \(seconds\): ([\d.]+)"), g(r"System time \(seconds\): ([\d.]+)"),
                 g(r"Percent of CPU this job got: (\S+)%"), g(r"Elapsed \(wall clock\) time[^\n]*: (\S+)"),
                 unc, res, fresh, rep, hits, str(int(du) // 1073741824)]))
PYEOF
	done
	python3 -I - "$OUT/local-cold-summary.tsv" <<'PYEOF'
import sys
rows = [l.split("\t") for l in open(sys.argv[1]).read().splitlines() if l.strip()]
if not rows:
    sys.exit("no local-cold summary rows")
med = lambda xs: xs[len(xs) // 2] if len(xs) % 2 else (xs[len(xs)//2 - 1] + xs[len(xs)//2]) / 2
for col, name in [(1, "wall_to_first_ingest_s"), (2, "wall_to_listening_s"), (3, "peak_rss_mib")]:
    xs = sorted(float(r[col]) for r in rows)
    print("median_%s\t%.1f" % (name, med(xs)))
    print("range_%s\t%.1f-%.1f" % (name, xs[0], xs[-1]))
print("runs\t%d" % len(rows))
PYEOF
	;;

*)
	echo "unknown cmd: $CMD" >&2
	exit 2
	;;
esac
