#!/bin/sh
# perf-export.sh run DATADIR SESSION N BIN_A BIN_B   alternating A,B,... runs of
#                `mnemonic export otlp` on one session
# perf-export.sh equal DATADIR IDS_FILE BIN_A BIN_B  sha256 of both binaries' output per session
#
# DATADIR is a COPY of a serve store (raw/ + index/); it is never written: this
# script holds index/writer.lock on its own fd 9 (no child process holds it), so export skips its settle-all ingest and
# exports the published index, as it does under a running server.
# run prints, per side: n, median and range of wall seconds and of peak RSS (MiB),
# the failed-run count, the bytes written and the number of distinct output shas
# (1 expected on every side, and equal across sides). equal prints the sessions
# compared and the number whose sha differs. Never prints output content.
# Run under the box lock: flock /var/lib/mnu/box.lock sh perf-export.sh ...
set -eu

mode=${1:?usage: perf-export.sh run|equal ...}
shift
TMP=$(mktemp -d /var/lib/mnu/out/p3-perf.XXXXXX)
cleanup() {
	rm -f "$TMP"/*.json "$TMP"/*.tm "$TMP"/*.err "$TMP"/*.tsv 2>/dev/null || true
	rmdir "$TMP" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

hold() {
	exec 9> "$1/index/writer.lock"
	flock -n 9 || { echo "writer.lock of $1 is held elsewhere" >&2; exit 2; }
}

stats() { # file of numbers -> n median min max
	sort -g "$1" | awk '{a[NR]=$1} END{m=(NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2; printf "n=%d median=%.2f range=%.2f..%.2f", NR, m, a[1], a[NR]}'
}

case $mode in
run)
	data=${1:?DATADIR} sid=${2:?SESSION} n=${3:?N} a=${4:?BIN_A} b=${5:?BIN_B}
	hold "$data"
	for i in $(seq 1 "$n"); do
		for side in a b; do
			eval bin=\$$side
			if /usr/bin/time -f '%e %M' -o "$TMP/$side.$i.tm" "$bin" export otlp --data-dir "$data" --session "$sid" --out "$TMP/$side.$i.json" 2>"$TMP/$side.$i.err"; then
				read -r wall kb < "$TMP/$side.$i.tm"
				echo "$wall" >> "$TMP/$side.wall.tsv"
				awk -v k="$kb" 'BEGIN{printf "%.0f\n", k/1024}' >> "$TMP/$side.rss.tsv"
				sha256sum "$TMP/$side.$i.json" | cut -d' ' -f1 >> "$TMP/$side.sha.tsv"
				stat -c %s "$TMP/$side.$i.json" > "$TMP/$side.bytes.tsv"
			else
				echo "run $i side $side failed" >&2
				echo "$side" >> "$TMP/fail.tsv"
			fi
			rm -f "$TMP/$side.$i.json"
		done
	done
	for side in a b; do
		fails=0
		[ -f "$TMP/fail.tsv" ] && fails=$(grep -c "^$side$" "$TMP/fail.tsv" || true)
		printf '%s wall_s %s | rss_MiB %s | failed=%s bytes=%s distinct_sha=%s\n' "$side" \
			"$(stats "$TMP/$side.wall.tsv")" "$(stats "$TMP/$side.rss.tsv")" "$fails" \
			"$(cat "$TMP/$side.bytes.tsv" 2>/dev/null || echo 0)" "$(sort -u "$TMP/$side.sha.tsv" | wc -l)"
	done
	echo "sha_equal_across_sides=$([ "$(sort -u "$TMP/a.sha.tsv" "$TMP/b.sha.tsv" | wc -l)" = 1 ] && echo yes || echo no)"
	;;
equal)
	data=${1:?DATADIR} ids=${2:?IDS_FILE} a=${3:?BIN_A} b=${4:?BIN_B}
	hold "$data"
	total=0 differ=0 failed=0
	while read -r sid _; do
		[ -n "$sid" ] || continue
		total=$((total + 1))
		ra=0 rb=0
		"$a" export otlp --data-dir "$data" --session "$sid" --out "$TMP/a.json" 2>"$TMP/a.err" || ra=$?
		"$b" export otlp --data-dir "$data" --session "$sid" --out "$TMP/b.json" 2>"$TMP/b.err" || rb=$?
		if [ "$ra" != 0 ] || [ "$rb" != 0 ]; then
			failed=$((failed + 1))
			# both sides refusing the same way is equal behaviour
			[ "$ra" = "$rb" ] && cmp -s "$TMP/a.err" "$TMP/b.err" && continue
		fi
		if [ "$(sha256sum < "$TMP/a.json" 2>/dev/null)" != "$(sha256sum < "$TMP/b.json" 2>/dev/null)" ]; then
			differ=$((differ + 1))
			echo "differs: session #$total" >&2
		fi
		rm -f "$TMP/a.json" "$TMP/b.json"
	done < "$ids"
	echo "sessions=$total failed_both_alike_or_not=$failed differ=$differ"
	[ "$differ" = 0 ]
	;;
*) echo "usage: perf-export.sh run|equal ..." >&2; exit 2 ;;
esac
