#!/bin/sh
set -eu
DIR=${1:?usage: hot-prep.sh DIR HOME_DIR}
HOME_DIR=${2:?usage: hot-prep.sh DIR HOME_DIR}
BIG=${BIG:-0.7 1.7 5 15 17 26 40}
SMALL=${SMALL:-0.1 0.15 0.2 0.25 0.3 0.35 0.4 0.45 0.5 0.55 0.6 0.65 0.7}
if [ -n "${SYNTH:-}" ]; then
	python3 -I - "$HOME_DIR" "$SYNTH" "${SYNTH_FROM:-20}" "${SYNTH_LINES:-200}" <<'PYEOF'
import json, os, sys, uuid
home, n, first, lines = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
for i in range(first, first + n):
    d = os.path.join(home, ".claude/projects", "hot-%02d" % i)
    os.makedirs(d)
    sid = str(uuid.uuid4())
    out = os.path.join(d, sid + ".jsonl")
    parent = None
    with open(out, "w") as f:
        for k in range(lines):
            u = str(uuid.uuid4())
            role = "user" if k % 2 == 0 else "assistant"
            f.write(json.dumps({"type": role, "uuid": u, "parentUuid": parent, "sessionId": sid,
                                "timestamp": "2026-10-26T12:%02d:%02dZ" % (k // 60 % 60, k % 60), "cwd": "/synth/%02d" % i, "version": "2.0.0",
                                "message": {"role": role, "content": "synthetic hot session %d line %d" % (i, k)}}) + "\n")
            parent = u
    print("hot-%02d\tsynthetic\tjsonl_bytes\t%d\tlines\t%d" % (i, os.path.getsize(out), lines))
PYEOF
	exit 0
fi
[ -e "$HOME_DIR/.claude/projects" ] && { echo "$HOME_DIR/.claude/projects exists" >&2; exit 1; }
mkdir -p "$HOME_DIR/.claude/projects"
python3 -I - "$DIR" "$HOME_DIR" "$BIG" "$SMALL" <<'PYEOF'
import glob, os, subprocess, sys
data, home, big, small = sys.argv[1:5]
cands = []
for z in glob.glob(os.path.join(data, "raw", "author=*", "machine=*", "harness=claude-code", "*.jsonl.zst")):
    cands.append((os.path.getsize(z), z))
cands.sort()
used = set()
targets = [float(x) for x in big.split()] + [float(x) for x in small.split()]
total = 0
for i, t in enumerate(targets):
    want = t * 1e6
    s, z = min((c for c in cands if c[1] not in used), key=lambda c: abs(c[0] - want))
    used.add(z)
    d = os.path.join(home, ".claude/projects", "hot-%02d" % i)
    os.makedirs(d)
    out = os.path.join(d, os.path.basename(z)[:-len(".zst")])
    with open(out, "wb") as f:
        subprocess.run(["zstd", "-dcq", z], stdout=f, check=True)
    n = sum(1 for _ in open(out, "rb"))
    size = os.path.getsize(out)
    total += size
    print("hot-%02d\ttarget_mb\t%.2f\tzst_bytes\t%d\tjsonl_bytes\t%d\tlines\t%d" % (i, t, s, size, n))
print("total_jsonl_bytes\t%d\tfiles\t%d" % (total, len(targets)))
PYEOF
