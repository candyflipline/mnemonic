#!/bin/sh
# hot-shape.sh DATA_DIR: the hot set of a store, sizes only (read only, safe
# on prod). Hot parts are CURRENT's "hot" list. A hot part holding delta.json
# answers through a delta over an image: the images are the dirs under
# index/deltas, sized by index/parts/<image>/{light,heavy}. Any other hot part
# is a full part, sized by its own light and heavy.
cd "${1:?usage: hot-shape.sh DATA_DIR}/index" || exit 1
n=0 nd=0 nf=0 fl=0 fh=0 ni=0 il=0 ih=0 m=0
for i in $(sed -n 's/.*"hot":\[\([^]]*\)\].*/\1/p' CURRENT | tr -d '"' | tr , ' '); do
	n=$((n + 1))
	if [ -f "parts/$i/delta.json" ]; then
		nd=$((nd + 1))
	elif s=$(stat -c %s "parts/$i/light" "parts/$i/heavy" 2>/dev/null); then
		nf=$((nf + 1)) fl=$((fl + $(echo "$s" | sed -n 1p))) fh=$((fh + $(echo "$s" | sed -n 2p)))
	else
		m=$((m + 1))
	fi
done
for i in $(ls deltas 2>/dev/null); do
	if s=$(stat -c %s "parts/$i/light" "parts/$i/heavy" 2>/dev/null); then
		ni=$((ni + 1)) il=$((il + $(echo "$s" | sed -n 1p))) ih=$((ih + $(echo "$s" | sed -n 2p)))
	else
		m=$((m + 1))
	fi
done
echo "hot $n delta_parts $nd images $ni image_light $il image_heavy $ih full_parts $nf full_light $fl full_heavy $fh light_bytes $((il + fl)) heavy_bytes $((ih + fh)) missing $m delta_kb $(du -sk deltas 2>/dev/null | cut -f1)"
