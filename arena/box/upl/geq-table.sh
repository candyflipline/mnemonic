#!/bin/sh
# geq-table.sh GLOB... — median, min, max, n of the numeric keys across geq summaries.
# Usage: geq-table.sh KEY 'out/geq-m32-c1-http-r*-summary.txt' ...  (one glob per row)
# Prints "<glob>\t<key>\tmedian\tmin\tmax\tn". Keys with several values per file
# (client_cpu_pct) are summed per file by taking the first field only.
set -eu
KEY=${1:?usage: geq-table.sh KEY GLOB...}
shift
for g in "$@"; do
	for f in $g; do
		[ -f "$f" ] && awk -F'\t' -v k="$KEY" '$1==k{split($2,a," "); print a[1]; exit}' "$f"
	done | sort -g | awk -v g="$g" -v k="$KEY" '{v[NR]=$1} END{ if (NR==0) {printf "%s\t%s\tn 0\n", g, k; exit}
		m=(NR%2)?v[(NR+1)/2]:(v[NR/2]+v[NR/2+1])/2; printf "%s\t%s\tmedian %s\tmin %s\tmax %s\tn %d\n", g, k, m, v[1], v[NR], NR }'
done
