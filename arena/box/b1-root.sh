#!/bin/sh
# b1-root.sh — lane B1: dataroots for the deploy-shape numbers.
#   root-ra  raw + archive: raw is the symlink itself, the 16 ch-*.parquet
#            hardlinked under events/ at their exact relative paths (the one
#            lane ARCH2 fixed: the engine root's raw is the symlink, one level
#            deep, so the archive files are seen).
#   root-r   raw only (the H/C append base store builds from this).
# Run under the box lock. New dirs only, under $BOX/b1/. Nothing is deleted:
# refuses to touch an existing root. Counts only; never prints corpus content.
set -eu
BOX=${BOX:-/var/lib/mnemonic-e3}
OUT=${OUT:-$BOX/out/b1}
B1=$BOX/b1

mkdir -p "$OUT"

# ch list (read-only source: the frozen archive copy)
CHL=$OUT/ch-list.txt
find "$BOX/archive/events" -name 'ch-*.parquet' | sort > "$CHL"
N=$(wc -l < "$CHL")
[ "$N" -eq 16 ] || { echo "FAIL: expected 16 ch-* files, found $N" >&2; exit 1; }
echo "ch files: $N"

# root-ra
if [ -e "$B1/root-ra" ]; then echo "FAIL: $B1/root-ra exists" >&2; exit 1; fi
mkdir -p "$B1/root-ra"
while IFS= read -r p; do
	rel=${p#"$BOX/archive/"}
	mkdir -p "$B1/root-ra/$(dirname "$rel")"
	ln "$p" "$B1/root-ra/$rel"
done < "$CHL"
ln -sfn "$BOX/raw" "$B1/root-ra/raw"
R=$(find "$B1/root-ra/events" -name 'ch-*.parquet' | wc -l)
[ "$R" -eq 16 ] || { echo "FAIL: root-ra holds $R ch files, want 16" >&2; exit 1; }
ls "$B1"/root-ra/raw/author=* > /dev/null 2>&1 || { echo "FAIL: root-ra/raw has no author=*" >&2; exit 1; }
ls -d "$B1"/root-ra/raw/author=*/* > /dev/null 2>&1 || { echo "FAIL: root-ra/raw/author=* does not resolve" >&2; exit 1; }
NSRC=$(find -L "$B1/root-ra/raw" -name '*.jsonl.zst' | wc -l)
echo "root-ra: raw symlink ok, $R ch files, $NSRC raw .zst files"

# root-r (raw only)
if [ -e "$B1/root-r" ]; then echo "FAIL: $B1/root-r exists" >&2; exit 1; fi
mkdir -p "$B1/root-r"
ln -sfn "$BOX/raw" "$B1/root-r/raw"
ls "$B1"/root-r/raw/author=* > /dev/null 2>&1 || { echo "FAIL: root-r/raw has no author=*" >&2; exit 1; }
echo "root-r: raw symlink ok (raw only)"
