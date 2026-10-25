#!/bin/sh
# b1-mpick.sh — lane B1: pick case M's target, a raw source sharing a session
# id with an archive member (a mixed raw+archive component), from a settled
# raw+archive store's checkpoints. Writes M-target / M-sid / M-lines.jsonl
# next to PERF's H/C case files. Identifiers go to the files only; stdout is
# counts. Nothing is printed from the corpus.
#   b1-mpick.sh STORE
set -eu
BOX=/var/lib/mnemonic-e3
P=/root/perf
STORE=${1:?usage: b1-mpick.sh STORE}
[ -f "$P/H-lines.jsonl" ] || { echo "FAIL: $P/H-lines.jsonl missing" >&2; exit 1; }
python3 -I - "$STORE/src" "$P" <<'PY'
import json, os, sys
src, p = sys.argv[1], sys.argv[2]
ARCH = (2, "archive")
RAWK = (0, "store")
arch_sids = set()
n_arch = 0
for f in os.listdir(src):
    if not f.endswith(".ckpt") or "~" in f:
        continue
    c = json.load(open(os.path.join(src, f)))
    if c.get("kind") in ARCH:
        n_arch += 1
        for s in c.get("sessions") or []:
            arch_sids.add(s)
        k = c.get("key") or f[:-5]
        arch_sids.add(k.split("__", 3)[-1])
picked = None
n_raw = 0
cands = 0
for f in sorted(os.listdir(src)):
    if not f.endswith(".ckpt") or "~" in f:
        continue
    c = json.load(open(os.path.join(src, f)))
    if c.get("kind") not in RAWK:
        continue
    n_raw += 1
    sids = [s for s in (c.get("sessions") or []) if s in arch_sids]
    if not sids:
        continue
    cands += 1
    a, m, h, sid = (c.get("key") or f[:-5]).split("__", 3)
    rel = "author=%s/machine=%s/harness=%s/%s.json" % (a, m, h, sid)
    if not os.path.exists("/var/lib/mnemonic-e3/b1/root-ra/raw/" + rel):
        continue
    picked = (rel, sid, c.get("rows"), len(sids))
    break
assert picked, "no raw source sharing a session id with an archive member"
rel, sid, rows, shared = picked
open(p + "/M-target", "w").write(rel + "\n")
open(p + "/M-sid", "w").write(sid + "\n")
out = open(p + "/M-lines.jsonl", "w")
for line in open(p + "/H-lines.jsonl"):
    o = json.loads(line)
    o["sessionId"] = sid
    out.write(json.dumps(o, separators=(",", ":"), ensure_ascii=False) + "\n")
print("M pick: raw ckpts=%d archive ckpts=%d mixed candidates=%d picked rows=%s shared_session_ids=%d" % (
    n_raw, n_arch, cands, rows, shared))
PY
ls -la "$P/M-target" "$P/M-sid" "$P/M-lines.jsonl" | awk '{print $5, $9}'
wc -l "$P/M-lines.jsonl"
