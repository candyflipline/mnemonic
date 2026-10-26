#!/bin/sh
# hot-shape.sh DATA_DIR: hot image count and their light and heavy bytes, from
# ls and stat only (read only, safe on prod). A hot component is a dir under
# index/deltas named for its image; the image is index/parts/<that name>.
# Prints one line: hot N light_bytes L heavy_bytes H missing M delta_kb D.
cd "${1:?usage: hot-shape.sh DATA_DIR}/index" || exit 1
n=0 l=0 h=0 m=0
for i in $(ls deltas); do
	n=$((n + 1))
	if s=$(stat -c %s "parts/$i/light" "parts/$i/heavy" 2>/dev/null); then
		l=$((l + $(echo "$s" | sed -n 1p)))
		h=$((h + $(echo "$s" | sed -n 2p)))
	else
		m=$((m + 1))
	fi
done
echo "hot $n light_bytes $l heavy_bytes $h missing $m delta_kb $(du -sk deltas | cut -f1)"
