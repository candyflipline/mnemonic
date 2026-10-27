#!/bin/sh
set -eu
A=${1:?usage: equiv.sh DIR_A DIR_B}
B=${2:?usage: equiv.sh DIR_A DIR_B}
MASKS=updated_ms
for d in "$A" "$B"; do [ -d "$d/raw" ] || { echo "no $d/raw" >&2; exit 2; }; done
exec python3 -I - "$A/raw" "$B/raw" "$MASKS" <<'PYEOF'
import hashlib, json, os, sys
from concurrent.futures import ThreadPoolExecutor
a, b, masks = sys.argv[1], sys.argv[2], [m for m in sys.argv[3].split(",") if m]
def walk(root):
    out = {}
    for d, _, fs in os.walk(root):
        for f in fs:
            p = os.path.join(d, f)
            out[os.path.relpath(p, root)] = p
    return out
fa, fb = walk(a), walk(b)
only_a, only_b = sorted(set(fa) - set(fb)), sorted(set(fb) - set(fa))
both = sorted(set(fa) & set(fb))
def sha(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for blk in iter(lambda: f.read(1 << 20), b""):
            h.update(blk)
    return h.digest()
def load(p):
    with open(p) as f:
        m = json.load(f)
    for k in masks:
        m[k] = 0 if isinstance(m.get(k), int) and not isinstance(m.get(k), bool) else "UNMASKABLE"
    return m
frames = [r for r in both if r.endswith(".zst")]
mans = [r for r in both if r.endswith(".json") and not os.path.basename(r).startswith(".")]
other = [r for r in both if r not in set(frames) and r not in set(mans)]
with ThreadPoolExecutor(max_workers=8) as ex:
    fdiff = [r for r, x, y in zip(frames, ex.map(sha, [fa[r] for r in frames]), ex.map(sha, [fb[r] for r in frames])) if x != y]
mdiff = []
mfield = {}
for r in mans:
    x, y = load(fa[r]), load(fb[r])
    if x != y:
        mdiff.append(r)
        for k in set(x) | set(y):
            if x.get(k) != y.get(k):
                mfield[k] = mfield.get(k, 0) + 1
odiff = [r for r in other if sha(fa[r]) != sha(fb[r])]
print("files_a\t%d\nfiles_b\t%d" % (len(fa), len(fb)))
print("frames_compared\t%d\tdiffering\t%d" % (len(frames), len(fdiff)))
print("manifests_compared\t%d\tdiffering_after_mask\t%d\tmasked\t%s" % (len(mans), len(mdiff), ",".join(masks) or "none"))
print("other_files\t%d\tdiffering\t%d" % (len(other), len(odiff)))
print("only_in_a\t%d\nonly_in_b\t%d" % (len(only_a), len(only_b)))
for k in sorted(mfield):
    print("manifest_field_differs\t%s\t%d" % (k, mfield[k]))
for name, xs in (("only_a", only_a), ("only_b", only_b), ("frame", fdiff), ("manifest", mdiff), ("other", odiff)):
    for r in xs[:5]:
        print("diff_%s\t%s" % (name, r))
ok = not (only_a or only_b or fdiff or mdiff or odiff)
print("EQUIVALENT" if ok else "DIFFERENT")
sys.exit(0 if ok else 1)
PYEOF
