#!/bin/sh
set -eu
KEY=${1:?usage: geq-table.sh KEY GLOB...}
shift
for g in "$@"; do
	for f in $g; do
		[ -f "$f" ] && awk -F'\t' -v k="$KEY" '$1==k{split($2,a," "); print a[1]; exit}' "$f"
	done | sort -g | awk -v g="$g" -v k="$KEY" '{v[NR]=$1} END{ if (NR==0) {printf "%s\t%s\tn 0\n", g, k; exit}
		m=(NR%2)?v[(NR+1)/2]:(v[NR/2]+v[NR/2+1])/2; printf "%s\t%s\tmedian %s\tmin %s\tmax %s\tn %d\n", g, k, m, v[1], v[NR], NR }'
done
