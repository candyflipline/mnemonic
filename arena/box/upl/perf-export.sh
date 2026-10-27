#!/bin/sh
# perf-export.sh STORE OLD NEW [ANNOTATION]
#
# Old `mnemonic export --data-dir` against the export route of a running
# serve, on one store. STORE is a COPY of a serve store; NEW serves it (under
# MemoryMax=8G, GOMAXPROCS=4, as prod) and so holds its writer lock, which
# makes OLD take its no-ingest branch and export the published index, as it
# would beside a live server. Picks 32 sessions from list_sessions by event
# count (the largest, every third of the top 40, 18 spread over the rest; env
# SKIP=n drops the first n of those), then
# per session and kind (atif, otlp) runs OLD and NEW alternately, OLD first on
# odd sessions, and compares sha256. With ANNOTATION it annotates the 32
# sessions with it through the annotate tool and compares one dataset run. Prints counts, sha agreement, wall and peak RSS (median, range,
# n) per side, serve VmHWM over an idle settle cycle and over the export
# window, and the serve cgroup's oom_kill. Never prints output content.
# Its numbers are timed: run under the exclusive box lock,
# flock -x /var/lib/mnu/box.lock sh perf-export.sh ...
set -eu

store=${1:?usage: perf-export.sh STORE OLD NEW [ANNOTATION]}
old=${2:?OLD binary}
new=${3:?NEW binary}
annotation=${4:-}
port=${PORT:-18840}
url=http://127.0.0.1:$port
TMP=$(mktemp -d /var/lib/mnu/out/export-perf.XXXXXX)
SERVE= SAMPLER= PIDSTAT=
cleanup() {
	set +e
	for p in $PIDSTAT $SAMPLER; do kill "$p" 2>/dev/null; done
	if [ -n "$SERVE" ]; then
		kill "$SERVE" 2>/dev/null
		for _ in $(seq 1 30); do kill -0 "$SERVE" 2>/dev/null || break; sleep 1; done
		kill -9 "$SERVE" 2>/dev/null
	fi
	rm -f "$TMP"/*.json "$TMP"/*.jsonl "$TMP"/*.err 2>/dev/null
}
trap cleanup EXIT
trap 'exit 1' INT TERM

head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$TMP/reader.tok"
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$TMP/admin.tok"
hash() { printf %s "$(cat "$1")" | sha256sum | cut -d' ' -f1; }
cat > "$TMP/serve.toml" <<EOF
data_dir = "$store"
listen = "127.0.0.1:$port"
push_listen = "127.0.0.1:$((port + 1))"
[[tokens]]
author = "export-probe"
sha256 = "$(hash "$TMP/reader.tok")"
scopes = ["read"]
[[tokens]]
author = "export-annotator"
sha256 = "$(hash "$TMP/admin.tok")"
scopes = ["read", "annotate"]
EOF

serves=$(pgrep -c -f ' serve --config ' || true)
avail=$(free -g | awk '/^Mem:/{print $7}')
if [ "$serves" -ge 3 ] || [ "$avail" -lt 10 ]; then
	echo "refusing to start serve: $serves serves running, ${avail} GiB available" >&2
	exit 1
fi
systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 env GOMAXPROCS=4 "$new" serve --config "$TMP/serve.toml" > "$TMP/serve.log" 2>&1 &
SERVE=$!
cg=/sys/fs/cgroup$(cut -d: -f3 "/proc/$SERVE/cgroup")
until grep -q 'cycle boot' "$TMP/serve.log"; do
	kill -0 "$SERVE" || { echo "serve exited at boot" >&2; exit 1; }
	sleep 2
done
echo "boot: $(grep 'cycle boot' "$TMP/serve.log" | sed 's/.* in /in /')"
hwm() { awk '/VmHWM/{printf "%.0f", $2/1024}' "/proc/$SERVE/status"; }
echo "serve VmHWM after boot MiB=$(hwm)"

mcp() {
	curl -sf -H 'MCP-Protocol-Version: 2025-06-18' -H "Authorization: Bearer $(cat "$TMP/reader.tok")" \
		-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d "$1" "$url/mcp"
}
cursor=
: > "$TMP/all.tsv"
while :; do
	args='{"limit":25,"start_datetime":"2000-01-01T00:00:00Z","end_datetime":"2100-01-01T00:00:00Z"'
	[ -n "$cursor" ] && args="$args,\"cursor\":\"$cursor\""
	page=$(mcp "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"list_sessions\",\"arguments\":$args}}}")
	printf %s "$page" | jq -r '.result.structuredContent.data.sessions[] | "\(.id)\t\(.session.event_count)"' >> "$TMP/all.tsv"
	cursor=$(printf %s "$page" | jq -r '.result.structuredContent.data.next_cursor // empty')
	[ -n "$cursor" ] || break
done
sort -t "$(printf '\t')" -k2,2nr -k1,1 "$TMP/all.tsv" | cut -f1 > "$TMP/ranked"
n=$(wc -l < "$TMP/ranked")
{
	sed -n 1p "$TMP/ranked"
	awk 'NR>=4 && NR<=40 && (NR-4)%3==0' "$TMP/ranked"
	awk -v n="$n" 'BEGIN{for(i=0;i<18;i++) pick[41+int(i*(n-41)/18)]=1} pick[NR]' "$TMP/ranked"
} | tail -n +$((${SKIP:-0} + 1)) > "$TMP/ids"
echo "listed=$n picked=$(wc -l < "$TMP/ids") distinct=$(sort -u "$TMP/ids" | wc -l)"

echo 5 > "/proc/$SERVE/clear_refs"
start=$(grep -c '^.*cycle ' "$TMP/serve.log" || true)
waited=0
while [ "$(grep -c '^.*cycle ' "$TMP/serve.log" || true)" = "$start" ] && [ "$waited" -lt "${IDLE:-330}" ]; do
	sleep 5
	waited=$((waited + 5))
done
echo "idle: waited ${waited}s, cycles $(grep -c '^.*cycle ' "$TMP/serve.log") ($(tail -1 "$TMP/serve.log" | sed 's/.*cycle \([a-z]*\).* in \([^ ]*\).*/\1 in \2/')); serve VmHWM idle MiB=$(hwm)"

echo 5 > "/proc/$SERVE/clear_refs"
(while kill -0 "$SERVE" 2>/dev/null; do
	awk '/VmRSS/{print $2}' "/proc/$SERVE/status"
	sleep 1
done) > "$TMP/serve-rss.kb" &
SAMPLER=$!
pidstat -u -p "$SERVE" 5 > "$TMP/serve-pidstat.log" 2>&1 &
PIDSTAT=$!

timed() { # side out cmd...: run cmd timed, append wall, RSS MiB, rc
	side=$1 outf=$2
	shift 2
	rc=0
	/usr/bin/time -f '%e %M' -o "$TMP/$side.tm" "$@" --out "$outf" 2>"$TMP/$side.err" || rc=$?
	read -r wall kb < "$TMP/$side.tm" || true
	echo "$wall" >> "$TMP/$side.wall"
	awk -v k="$kb" 'BEGIN{printf "%.1f\n", k/1024}' >> "$TMP/$side.rss"
	[ "$rc" = 0 ] || echo "$side" >> "$TMP/fail"
	return 0
}
runold() { timed old "$TMP/old.json" "$old" export "$@" --data-dir "$store"; }
runnew() { timed new "$TMP/new.json" "$new" export "$@" --server "$url" --token-file "$TMP/reader.tok"; }

i=0 same=0 differ=0 both_fail=0 one_fail=0 bytes=0
while read -r sid; do
	i=$((i + 1))
	for kind in atif otlp; do
		: > "$TMP/fail"
		rm -f "$TMP/old.json" "$TMP/new.json"
		if [ $((i % 2)) = 1 ]; then
			runold "$kind" --session "$sid"
			runnew "$kind" --session "$sid"
		else
			runnew "$kind" --session "$sid"
			runold "$kind" --session "$sid"
		fi
		f=$(wc -l < "$TMP/fail")
		if [ "$f" = 2 ]; then
			both_fail=$((both_fail + 1))
		elif [ "$f" = 1 ]; then
			one_fail=$((one_fail + 1))
			echo "one side failed: session #$i $kind ($(cat "$TMP/fail"))" >&2
		elif [ "$(sha256sum < "$TMP/old.json")" = "$(sha256sum < "$TMP/new.json")" ]; then
			same=$((same + 1))
			bytes=$((bytes + $(stat -c %s "$TMP/new.json")))
		else
			differ=$((differ + 1))
			echo "differs: session #$i $kind" >&2
		fi
	done
done < "$TMP/ids"
echo "atif+otlp: compared=$((same + differ + both_fail + one_fail)) same=$same differ=$differ both_failed=$both_fail one_side_failed=$one_fail bytes_same=$bytes"

if [ -n "$annotation" ]; then
	before=$(grep -c 'cycle put' "$TMP/serve.log" || true)
	ok=0
	while read -r sid; do
		r=$(curl -sf -H 'MCP-Protocol-Version: 2025-06-18' -H "Authorization: Bearer $(cat "$TMP/admin.tok")" \
			-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
			-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"annotate\",\"arguments\":{\"session_id\":\"$sid\",\"name\":\"$annotation\",\"number\":1}}}" "$url/mcp" |
			jq -r '.result.isError // false')
		[ "$r" = false ] && ok=$((ok + 1))
	done < "$TMP/ids"
	waited=0
	while [ "$(grep -c 'cycle put' "$TMP/serve.log" || true)" = "$before" ] && [ "$waited" -lt 60 ]; do
		sleep 1
		waited=$((waited + 1))
	done
	echo "annotated=$ok put_cycle_after=${waited}s"
	: > "$TMP/fail"
	rm -f "$TMP/old.json" "$TMP/new.json"
	runold dataset --annotation "$annotation" --per-task 3
	runnew dataset --annotation "$annotation" --per-task 3
	echo "dataset: failed=$(wc -l < "$TMP/fail") lines_old=$(wc -l < "$TMP/old.json" 2>/dev/null || echo -) lines_new=$(wc -l < "$TMP/new.json" 2>/dev/null || echo -) sha_equal=$([ "$(sha256sum < "$TMP/old.json")" = "$(sha256sum < "$TMP/new.json")" ] && echo yes || echo no) skipped_old=$(grep -c 'skipped session' "$TMP/old.err" || true) skipped_new=$(grep -c 'skipped session' "$TMP/new.err" || true)"
fi

echo "serve VmHWM export window MiB=$(hwm) sampled_peak_RSS_MiB=$(sort -n "$TMP/serve-rss.kb" | tail -1 | awk '{printf "%.0f", $1/1024}') samples=$(wc -l < "$TMP/serve-rss.kb")"
echo "serve cgroup $(tr '\n' ' ' < "$cg/memory.events")"
stats() {
	sort -g "$1" | awk '{a[NR]=$1} END{m=(NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2; printf "n=%d median=%.2f range=%.2f..%.2f", NR, m, a[1], a[NR]}'
}
for side in old new; do
	echo "$side wall_s $(stats "$TMP/$side.wall") | peak_rss_MiB $(stats "$TMP/$side.rss")"
done
kill -INT "$PIDSTAT" 2>/dev/null || true
sleep 1
awk '/Average/ && $0 ~ /[0-9]/ {print "serve pidstat average %usr=" $4 " %system=" $5 " %CPU=" $8}' "$TMP/serve-pidstat.log"
echo "evidence: $TMP"
