#!/bin/sh
set -eu
DEN=${1:?usage: geq-subset.sh DEN NAME}
NAME=${2:?usage: geq-subset.sh DEN NAME}
BOX=${BOX:-/var/lib/mnu}
PH=${PH:-$BOX/plainhome}
G=$BOX/geqhome
[ -e "$G/$NAME-all" ] && { echo "$G/$NAME-all exists" >&2; exit 1; }
mkdir -p "$G"
python3 -I - "$PH" "$G" "$NAME" "$DEN" <<'PYEOF'
import os, sys
ph, g, name, den = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
files = []
for d, _, fs in os.walk(ph):
    for f in fs:
        p = os.path.join(d, f)
        files.append((os.path.getsize(p), p))
files.sort(key=lambda t: (-t[0], t[1]))
kept = files[den // 2::den]
tot = [0, 0, 0, 0]
cnt = [0, 0, 0, 0]
for i, (sz, p) in enumerate(kept):
    rel = os.path.relpath(p, ph)
    q = tot.index(min(tot))
    for dest in (name + "-all", "%s-q%d" % (name, q + 1)):
        t = os.path.join(g, dest, rel)
        os.makedirs(os.path.dirname(t), exist_ok=True)
        os.link(p, t)
    tot[q] += sz
    cnt[q] += 1
print("input_files\t%d\ninput_bytes\t%d" % (len(files), sum(s for s, _ in files)))
print("kept_files\t%d\nkept_bytes\t%d" % (len(kept), sum(s for s, _ in kept)))
for q in range(4):
    print("q%d\tfiles %d\tbytes %d" % (q + 1, cnt[q], tot[q]))
PYEOF
