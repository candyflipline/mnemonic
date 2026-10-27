#!/bin/sh
set -eu
STORE=${1:?usage: perf-open.sh STORE IDS BIN_A [BIN_B]}
IDS=${2:?ids file}
shift 2
[ $# -ge 1 ] || { echo "need BIN_A" >&2; exit 2; }
N=${N:-5}
OUT=${OUT:-/var/lib/mnu/out/perf-open-$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$OUT"
TIME=/usr/bin/time.gnu
[ -x "$TIME" ] || TIME=/usr/bin/time

REQ=$OUT/req.jsonl
: > "$REQ"
c=0
while read -r id; do
	[ -n "$id" ] || continue
	c=$((c + 1))
	printf '{"id":"s%d","tool":"open","args":{"id":"%s"}}\n' "$c" "$id" >> "$REQ"
	printf '{"id":"p%d","tool":"open","args":{"id":"%s","limit":25}}\n' "$c" "$id" >> "$REQ"
done < "$IDS"

ROWS=$OUT/rows.tsv
: > "$ROWS"
free -g | awk '/^Mem/ {print "mem_available_gb\t" $7}' > "$OUT/env.txt"
uptime >> "$OUT/env.txt"
for b in "$@"; do echo "bin	$(sha256sum "$b" | cut -c1-16)	$b" >> "$OUT/env.txt"; done

i=0
while [ "$i" -lt "$N" ]; do
	i=$((i + 1))
	for b in "$@"; do
		tag=$(sha256sum "$b" | cut -c1-8)
		rep=$OUT/run-$tag-$i.reply
		rc=0
		$TIME -f '%e %M' -o "$OUT/run-$tag-$i.time" "$b" index batch -store "$STORE" < "$REQ" > "$rep" 2> "$OUT/run-$tag-$i.err" || rc=$?
		read -r wall rss < "$OUT/run-$tag-$i.time"
		jq -r --arg tag "$tag" --arg run "$i" --arg rc "$rc" --arg wall "$wall" --arg rss "$rss" '
			[$tag, $run, .id, (.us|tostring),
			 (if (.error // "") == "" and $rc == "0" then "0" else "1" end),
			 ((.data.session.event_count // -1)|tostring), $wall, $rss] | @tsv' "$rep" > "$OUT/run-$tag-$i.rows"
		jq -c '{data, text}' "$rep" | while read -r l; do printf '%s' "$l" | sha256sum | cut -c1-16; done > "$OUT/run-$tag-$i.sha"
		paste "$OUT/run-$tag-$i.rows" "$OUT/run-$tag-$i.sha" >> "$ROWS"
		rm -f "$rep"
	done
done

awk -F'\t' '
	{ k = $1 "\t" $3; v[k, ++n[k]] = $4 / 1000; e[k] += $5; ev[k] = $6; w[k, n[k]] = $7; r[k] = ($8 > r[k] ? $8 : r[k]) }
	END {
		for (k in n) {
			m = n[k]
			for (a = 1; a <= m; a++) { s[a] = v[k, a]; t[a] = w[k, a] }
			for (a = 1; a <= m; a++) for (b = a + 1; b <= m; b++) {
				if (s[b] < s[a]) { x = s[a]; s[a] = s[b]; s[b] = x }
				if (t[b] < t[a]) { x = t[a]; t[a] = t[b]; t[b] = x }
			}
			med = (m % 2) ? s[(m + 1) / 2] : (s[m / 2] + s[m / 2 + 1]) / 2
			wm = (m % 2) ? t[(m + 1) / 2] : (t[m / 2] + t[m / 2 + 1]) / 2
			printf "%s\tn %d\tmedian_ms %.1f\tmin_ms %.1f\tmax_ms %.1f\terrors %d\tevent_count %s\tproc_wall_median_s %.2f\tmaxrss_kb %d\n", k, m, med, s[1], s[m], e[k], ev[k], wm, r[k]
		}
	}' "$ROWS" | sort > "$OUT/summary.tsv"
awk -F'\t' '{ seen[$3, $9] = 1; calls[$3] = 1 } END {
	d = 0
	for (c in calls) { k = 0; for (p in seen) { split(p, q, SUBSEP); if (q[1] == c) k++ } if (k != 1) d++; printf "answers\t%s\tdistinct %d\n", c, k }
	printf "calls_differing\t%d\n", d
}' "$ROWS" | sort >> "$OUT/summary.tsv"
cat "$OUT/summary.tsv"
echo "out	$OUT"
