#!/bin/sh
set -eu
ROUNDS=${1:?usage: geq-matrix.sh ROUNDS DEN}
DEN=${2:?usage: geq-matrix.sh ROUNDS DEN}
BOX=${BOX:-/var/lib/mnu}
OUT=${OUT:-$BOX/out}
HTTP_BIN=${HTTP_BIN:-$BOX/base-bin/mnemonic-83b0042}
STREAM_BIN=${STREAM_BIN:-$BOX/geq-bin/mnemonic-3829b3c}
SRC=${SRC:-$(dirname "$0")}
G=$BOX/geqhome/s$DEN
M=$BOX/geqm
mkdir -p "$M"
PORT=7850
APP_c1=; APP_c4=
r=1
while [ "$r" -le "$ROUNDS" ]; do
	if [ $((r % 2)) = 1 ]; then ORDER="http stream"; else ORDER="stream http"; fi
	for cond in c1 c4; do
		if [ $cond = c1 ]; then HOMES="$G-all"; else HOMES="$G-q1 $G-q2 $G-q3 $G-q4"; fi
		for side in $ORDER; do
			if [ $side = http ]; then BIN=$HTTP_BIN; else BIN=$STREAM_BIN; fi
			name=m$DEN-$cond-$side-r$r
			eval "app=\$APP_$cond"
			PORT=$((PORT + 1))
			APPENDS=$app HOMES="$HOMES" PORT=$PORT PROJECTED_GB=$((DEN > 16 ? 2 : 4)) sh "$SRC/geq-push.sh" "$BIN" "$M/$name" > "$OUT/geq-$name.stdout" 2>&1 || { echo "FAILED $name"; tail -5 "$OUT/geq-$name.stdout"; exit 1; }
			if [ $side = http ] && [ "$r" = 1 ]; then
				p=$(awk -F'\t' '$1=="puts"{print $2}' "$OUT/geq-$name-summary.txt")
				eval "APP_$cond=$p"
			fi
			echo "done $name $(awk -F'\t' '$1=="push_wall_s"{print $2}' "$OUT/geq-$name-summary.txt") s"
		done
		if sh "$SRC/equiv.sh" "$M/m$DEN-$cond-http-r$r" "$M/m$DEN-$cond-stream-r$r" > "$OUT/geq-m$DEN-$cond-r$r-equiv.txt" 2>&1; then
			echo "EQUIVALENT m$DEN-$cond-r$r"
		else
			echo "NOT EQUIVALENT m$DEN-$cond-r$r"; cat "$OUT/geq-m$DEN-$cond-r$r-equiv.txt"; exit 1
		fi
	done
	r=$((r + 1))
done
