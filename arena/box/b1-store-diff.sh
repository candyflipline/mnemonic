#!/bin/sh
# b1-store-diff.sh — lane B1: final store bytes by section.
#   parts by file name (and the legacy dcfe552 buckets: light = light,
#   heavy = heavy, hot = hot+hot.off+docs, spans = spans+annotations+authors,
#   postings = postings+terms+terms.idx+terms.filter; everything else listed
#   as its own name),
#   src by .ckpt/.light/.heavy and by raw vs archive (the ckpt's kind field),
#   CURRENT, sources fresh count (ckpt count by kind), archive log bytes left.
#   b1-store-diff.sh STORE TAG
# Counts and sizes only.
set -eu
STORE=${1:?usage: b1-store-diff.sh STORE TAG}
TAG=${2:?TAG}
python3 -I - "$STORE" "$TAG" <<'PY'
import json, os, sys
store, tag = sys.argv[1], sys.argv[2]
def du(path):
    t = 0
    for root, _, files in os.walk(path):
        for f in files:
            try:
                t += os.lstat(os.path.join(root, f)).st_size
            except OSError:
                pass
    return t
parts = os.path.join(store, "parts")
byname = {}
perpart = {}
for d in sorted(os.listdir(parts)):
    dd = os.path.join(parts, d)
    if not os.path.isdir(dd):
        continue
    t = 0
    for root, _, files in os.walk(dd):
        for f in files:
            n = os.path.join(root, f)
            s = os.lstat(n).st_size
            byname[f] = byname.get(f, 0) + s
            t += s
    perpart[d] = t
bucket = {"light": "light", "heavy": "heavy", "hot": "hot", "hot.off": "hot",
          "docs": "hot", "spans": "spans", "annotations": "spans",
          "authors": "spans", "postings": "postings", "terms": "postings",
          "terms.idx": "postings", "terms.filter": "postings"}
bk = {}
other = 0
for f, s in byname.items():
    if f in bucket:
        bk[bucket[f]] = bk.get(bucket[f], 0) + s
    else:
        other += s
ptotal = sum(bk.values()) + other
print("STORE %s parts_total %d" % (tag, ptotal))
for k in ("light", "heavy", "hot", "spans", "postings"):
    print("STORE %s part_bucket %s %d" % (tag, k, bk.get(k, 0)))
print("STORE %s part_bucket other %d" % (tag, other))
for f, s in sorted(byname.items(), key=lambda kv: -kv[1]):
    print("STORE %s part_file %s %d" % (tag, f, s))
print("STORE %s part_dirs %d" % (tag, len(perpart)))
src = os.path.join(store, "src")
rows = {}
for f in sorted(os.listdir(src)):
    p = os.path.join(src, f)
    if not os.path.isfile(p):
        continue
    s = os.lstat(p).st_size
    kind, key = "?", ""
    if f.endswith(".ckpt"):
        try:
            c = json.load(open(p))
            kind = str(c.get("kind"))
            key = c.get("key") or f[:-5]
        except Exception:
            pass
    cls = "archive" if kind in ("2", "archive") else ("raw" if kind in ("0", "store") else "other")
    ext = f.rsplit(".", 1)[-1]
    rows[(cls, ext)] = rows.get((cls, ext), 0) + s
    rows[(cls, "all")] = rows.get((cls, "all"), 0) + s
    if cls == "archive" and ext in ("light", "heavy"):
        rows[("archive", "logs_left")] = rows.get(("archive", "logs_left"), 0) + s
for (cls, ext), s in sorted(rows.items()):
    print("STORE %s src %s.%s %d" % (tag, cls, ext, s))
nk = {}
for f in sorted(os.listdir(src)):
    if f.endswith(".ckpt"):
        try:
            k = str(json.load(open(os.path.join(src, f))).get("kind"))
        except Exception:
            k = "?"
        nk[k] = nk.get(k, 0) + 1
print("STORE %s ckpts %s" % (tag, " ".join("kind%s=%d" % (k, v) for k, v in sorted(nk.items()))))
print("STORE %s CURRENT %d" % (tag, os.lstat(os.path.join(store, "CURRENT")).st_size))
print("STORE %s store_total %d" % (tag, du(store)))
PY
