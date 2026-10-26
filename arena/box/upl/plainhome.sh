#!/bin/sh
# plainhome.sh — build the plain client HOME from the frozen raw copy, once.
#
# Decompresses every raw source (RAW/author=*/machine=*/harness=*/<src>.jsonl.zst)
# to the harness-native path config.DefaultSources expects under a HOME, using
# the path its manifest carries, so `HOME=$PH mnemonic push` globs it as a
# developer machine's. A source whose native path is already claimed (two
# authors or machines on one path) goes to a distinct project dir
# (<root>/upl-N/...) under the same glob root; nothing is overwritten. Sources
# no DefaultSources root matches (server-owned annotation/control) are counted
# as unmapped and skipped. Counters only; no corpus content is printed.
#
# Adapted from arena/box/local-cold.sh `build` (lane/engine-box3 ddb22db).
#
# Env: BOX (default /var/lib/mnu); RAW (default $BOX/raw, read only);
#      PH (default $BOX/plainhome, must not exist); FREEFLOOR_GB (default 60):
#      refuse to start when the projected tree would leave less free on /.
# Writes $BOX/out/base-plainhome-stats.txt. Run under the box lock.
set -eu

BOX=${BOX:-/var/lib/mnu}
RAW=${RAW:-$BOX/raw}
PH=${PH:-$BOX/plainhome}
OUT=$BOX/out
FREEFLOOR_GB=${FREEFLOOR_GB:-60}
PROJECTED_GB=${PROJECTED_GB:-25}
mkdir -p "$OUT"
[ -e "$PH" ] && { echo "plainhome exists: $PH (built once; nothing deletes)" >&2; exit 1; }

free_gb=$(df -B1G --output=avail / | tail -1 | tr -d ' ')
[ $((free_gb - PROJECTED_GB)) -ge "$FREEFLOOR_GB" ] || {
	echo "free ${free_gb} GB minus ${PROJECTED_GB} GB projected is under the ${FREEFLOOR_GB} GB floor" >&2
	exit 1
}

cat > "$OUT/base-roots.tsv" <<'ROOTS'
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
ROOTS

python3 -I - "$RAW" "$PH" "$OUT/base-roots.tsv" "$OUT/base-plainhome-stats.txt" <<'PYEOF'
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
per_harness = {}
jobs = []

def target_for(m):
    global coll, unmapped
    root = roots.get((m.get("harness", ""), m.get("source_name", "")))
    path = m.get("path", "")
    if root is None or ("/" + root) not in path:
        unmapped += 1
        return None
    suffix = path[path.index("/" + root) + 1:]
    t = os.path.join(ph, suffix)
    if t in claimed or os.path.exists(t):
        coll += 1
        n = coll
        while True:
            t = os.path.join(ph, root, "upl-%d" % n, suffix[len(root):])
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
        jobs.append((mp[:-5] + ".jsonl.zst", t, int(m.get("size", -1))))

def decode(job):
    zst, t, want = job
    os.makedirs(os.path.dirname(t), 0o755, exist_ok=True)
    with open(zst, "rb") as f, open(t, "wb") as out:
        p = subprocess.Popen(["zstd", "-d", "-c"], stdin=f, stdout=out, stderr=subprocess.DEVNULL)
        if p.wait() != 0:
            return 1
    return 0 if os.path.getsize(t) == want else 2

with ThreadPoolExecutor(max_workers=8) as ex:
    res = list(ex.map(decode, jobs))
bad, sizemis = res.count(1), res.count(2)
total = sum(os.path.getsize(t) for _, t, _ in jobs)
with open(statpath, "w") as out:
    print("manifests\t%d" % len(mans), file=out)
    print("files_written\t%d" % len(jobs), file=out)
    print("collisions\t%d" % coll, file=out)
    print("unmapped\t%d" % unmapped, file=out)
    print("zstd_failures\t%d" % bad, file=out)
    print("size_vs_manifest_mismatch\t%d" % sizemis, file=out)
    print("bytes\t%d" % total, file=out)
    for h in sorted(per_harness):
        print("harness\t%s\t%d" % (h, per_harness[h]), file=out)
print(open(statpath).read(), end="")
sys.exit(1 if bad else 0)
PYEOF
du -sb "$PH"
