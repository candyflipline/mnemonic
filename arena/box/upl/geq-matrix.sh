#!/bin/sh
# geq-matrix.sh ROUNDS DEN — HTTP 83b0042 against the stream binary, alternating.
#
# Each round runs, for the 1-client and the 4-client condition, one HTTP push and
# one stream push of the same sub-home (geq-subset.sh DEN "s$DEN": -all for one
# client, -q1..q4 for four at once), the side order swapped every round (HS, SH,
# HS ...). Every pair is checked with equiv.sh (same home, so same ids): a pair
# that is not EQUIVALENT stops the matrix with exit 1. The stream run's APPENDS is
# the puts of the HTTP run of the same condition in round 1. Stores land in
# $BOX/geqm and are not deleted (list them for the owner).
# Output: $OUT/geq-m<DEN>-<cond>-<side>-r<N>-summary.txt, table via geq-table.sh.
# Run under the box lock, once: flock /var/lib/mnu/box.lock sh geq-matrix.sh 5 32
# Env: BOX, HTTP_BIN, STREAM_BIN, SRC (this directory).
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
