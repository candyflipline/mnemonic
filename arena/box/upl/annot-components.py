#!/usr/bin/env python3
import json, os, sys, collections

store = sys.argv[1]
src = os.path.join(store, "index", "src")
cks = []
for n in os.listdir(src):
    if not n.endswith(".ckpt"):
        continue
    c = json.load(open(os.path.join(src, n)))
    if c.get("harness") == "annotation":
        continue
    if "archive-" in c["key"] and "~" not in c["key"]:
        continue
    cks.append(c)

parent = list(range(len(cks)))
def find(i):
    while parent[i] != i:
        parent[i] = parent[parent[i]]
        i = parent[i]
    return i
owner = {}
for i, c in enumerate(cks):
    for s in c.get("sessions") or []:
        if s in owner:
            parent[find(i)] = find(owner[s])
        else:
            owner[s] = i

comp = collections.defaultdict(lambda: [0, 0, 0, 0])
for i, c in enumerate(cks):
    r = comp[find(i)]
    r[0] += 1
    r[1] += c.get("rows") or 0
    r[2] += (c.get("light_len") or 0) + (c.get("heavy_len") or 0)
    r[3] += len(c.get("sessions") or [])
size = sorted((v[2] for v in comp.values()), reverse=True)
print("sources=%d components=%d sessions=%d" % (len(cks), len(comp), len(owner)))
def pct(p):
    return size[min(len(size) - 1, int(len(size) * (1 - p)))]
print("component log bytes: p50=%d p90=%d p99=%d max=%d" % (pct(.5), pct(.9), pct(.99), size[0]))
rows = sorted((v[1] for v in comp.values()), reverse=True)
def rpct(p):
    return rows[min(len(rows) - 1, int(len(rows) * (1 - p)))]
print("component rows: p50=%d p90=%d p99=%d max=%d total=%d" % (rpct(.5), rpct(.9), rpct(.99), rows[0], sum(rows)))
print("top 5 components (sources rows log_bytes sessions):")
for v in sorted(comp.values(), key=lambda v: -v[2])[:5]:
    print("  ", *v)
tot = sum(size)
print("components over 100 MiB: %d holding %.1f%% of all log bytes" % (
    sum(1 for s in size if s > 100 << 20), 100.0 * sum(s for s in size if s > 100 << 20) / tot))
if len(sys.argv) > 2:
    print("annotated sessions (sources rows log_bytes sessions of their component):")
    for sid in open(sys.argv[2]).read().split():
        if sid in owner:
            print("  ", *comp[find(owner[sid])])
