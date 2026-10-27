#!/bin/sh
set -eu
B=${1:?usage: memcap-eq-compare.sh BASE-COPY FIX-COPY}
F=${2:?usage: memcap-eq-compare.sh BASE-COPY FIX-COPY}
OUT=${OUT:-/var/lib/mnu/out/memcap-c}
mkdir -p "$OUT"
bn=$(basename "$B")
fn=$(basename "$F")
for side in "$B:$bn" "$F:$fn"; do
	dir=${side%%:*}
	name=${side##*:}
	(cd "$dir/index/parts" && find . -type f -newer "$OUT/stamp-$name" -print0 | sort -z | xargs -0 sha256sum) \
		> "$OUT/hashes-$name.txt"
	echo "$name: $(wc -l < "$OUT/hashes-$name.txt") files hashed"
done
if diff "$OUT/hashes-$bn.txt" "$OUT/hashes-$fn.txt" > "$OUT/hashes.diff"; then
	echo "HASHES-EQUAL"
else
	echo "HASHES-DIFFER (see hashes.diff)"
	cat "$OUT/hashes.diff"
fi
python3 - "$B" "$F" "$OUT" "$bn" "$fn" <<'EOF'
import json, sys
b, f, out, bn, fn = sys.argv[1:6]
load = lambda s: json.load(open(s + "/index/CURRENT"))
jb, jf = load(b), load(f)
for name, d in ((bn, jb), (fn, jf)):
    for s in d.get("sources", []):
        s["updated_ms"] = 0
    json.dump(d, open(f"{out}/current-{name}.json", "w"), sort_keys=True, indent=1)
key = lambda s: (s["author"], s["machine"], s["harness"], s["size"], s["events"])
if jb == jf:
    print("CURRENT-EQUAL modulo updated_ms")
else:
    print("CURRENT-DIFFERS: parts_equal=%s hot_equal=%s sources_equal_modulo_updated_ms=%s (%d vs %d sources)" % (
        jb["parts"] == jf["parts"], jb.get("hot") == jf.get("hot"),
        sorted(map(key, jb["sources"])) == sorted(map(key, jf["sources"])),
        len(jb["sources"]), len(jf["sources"])))
EOF
