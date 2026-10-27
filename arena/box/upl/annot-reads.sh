#!/bin/sh
# annot-reads.sh STORE BIN SESSIONS NCALLS LABEL [OUTDIR]
#
# The read-diff capture for the annotate fix. On a settled full-store copy
# STORE with no annotation source, starts a serve (MemoryMax 8G, as prod),
# optionally makes NCALLS annotate calls (session i mod lines of SESSIONS,
# name annot-probe-i, number i, as annot-cycle.sh), waits for the put cycle
# they cause, then for each session in SESSIONS writes one file per tool to
# OUTDIR, hashed and never printed:
#   get_trace      the annotations array, every "ts" field deleted (call time)
#   query          the annotations relation rows for the target session
#                  (every column but ts, sorted by annotation_id)
#   list_sessions  the session's one entry, from a +-2 h window around its
#                  last span end (list_sessions has no id filter)
#   open           the first page of the session
#   search         the session ids search_sessions returns for "annot-probe",
#                  sorted (store-wide, so identical within one run)
# Then it puts STORE back as annot-cycle.sh does: stops serve, removes the
# annotation source's raw files (single files, never recursive), boots a
# second serve to record the reverse rebuild, stops it. NCALLS=0 is the
# control run: reads only, no annotate calls, no reset boot.
# Run under `flock -s /var/lib/mnu/box.lock` (correctness). Prints counts and
# hashes only; session ids and corpus content stay in OUTDIR on the box.
set -eu
store=${1:?usage: annot-reads.sh STORE BIN SESSIONS NCALLS LABEL [OUTDIR]}
bin=${2:?BIN}
sessions=${3:?SESSIONS file, one id per line}
ncalls=${4:?NCALLS}
label=${5:?LABEL}
out=${6:-/var/lib/mnu/annotbox-reads/$label}
port=${PORT:-18861}
url=http://127.0.0.1:$port
mkdir -p "$out"
TMP=$(mktemp -d /var/lib/mnu/annot-reads.XXXXXX)
SERVE=
stop_serve() {
	set +e
	if [ -n "$SERVE" ]; then
		kill "$SERVE" 2>/dev/null
		for _ in $(seq 1 120); do kill -0 "$SERVE" 2>/dev/null || break; sleep 1; done
		kill -9 "$SERVE" 2>/dev/null
		SERVE=
	fi
	set -e
}
cleanup() { stop_serve; rm -f "$TMP"/*.tok 2>/dev/null; }
trap cleanup EXIT
trap 'exit 1' INT TERM

head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$TMP/tok"
hash=$(printf %s "$(cat "$TMP/tok")" | sha256sum | cut -d' ' -f1)
cat > "$TMP/serve.toml" <<EOF
data_dir = "$store"
listen = "127.0.0.1:$port"
push_listen = "127.0.0.1:$((port + 1))"
[[tokens]]
author = "export-annotator"
sha256 = "$hash"
scopes = ["read", "annotate"]
EOF

start_serve() { # log
	serves=$(pgrep -c -f ' serve --config ' || true)
	avail=$(free -g | awk '/^Mem:/{print $7}')
	if [ "$serves" -ge 3 ] || [ "$avail" -lt 10 ]; then
		echo "refusing to start serve: $serves serves running, ${avail} GiB available" >&2
		exit 1
	fi
	systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 env GOMAXPROCS=4 "$bin" serve --config "$TMP/serve.toml" > "$1" 2>&1 &
	SERVE=$!
	until grep -q 'cycle boot' "$1"; do
		kill -0 "$SERVE" 2>/dev/null || { echo "serve exited at boot" >&2; cat "$1" >&2; exit 1; }
		sleep 2
	done
}

mcp() { # tool arguments outfile
	if curl -sf -H 'MCP-Protocol-Version: 2025-06-18' -H "Authorization: Bearer $(cat "$TMP/tok")" \
		-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
		-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}" \
		"$url/mcp" > "$3" 2>/dev/null; then
		return 0
	fi
	echo '{"curl_failed":true}' > "$3"
	return 1
}

log=$out/serve.log
start_serve "$log"
echo "$label boot: $(grep 'cycle boot' "$log" | sed 's/.*cycle boot: //')"
pcount() { grep -c 'cycle put' "$1" || true; }
before=$(pcount "$log")

nlines=$(wc -l < "$sessions")
ok=0 fail=0 dup=0
if [ "$ncalls" -gt 0 ]; then
	i=0
	while [ "$i" -lt "$ncalls" ]; do
		i=$((i + 1))
		sid=$(sed -n "$(((i - 1) % nlines + 1))p" "$sessions")
		if mcp annotate "{\"session_id\":\"$sid\",\"name\":\"annot-probe-$i\",\"number\":$i}" "$TMP/ann.json"; then
			e=$(jq -r '.result.isError // "x"' "$TMP/ann.json")
			d=$(jq -r '.result.structuredContent.data.duplicate // false' "$TMP/ann.json")
		else
			e=x d=false
		fi
		if [ "$e" = false ]; then ok=$((ok + 1)); else fail=$((fail + 1)); fi
		[ "$d" = true ] && dup=$((dup + 1))
	done
	waited=0
	while [ "$(pcount "$log")" = "$before" ] && [ "$waited" -lt 900 ]; do
		sleep 1
		waited=$((waited + 1))
	done
	sleep 20
fi
echo "$label annotate calls=$ncalls ok=$ok failed=$fail duplicate=$dup put_cycles=$(($(pcount "$log") - before))"
grep 'cycle put' "$log" | tail -n +$((before + 1)) | sed 's/.*cycle put: /put: /' || true

FROM=$(date -u -d '-20 days' +%Y-%m-%dT00:00:00Z)
TO=$(date -u -d '+1 days' +%Y-%m-%dT00:00:00Z)
for tool in get_trace query list_sessions open search; do mkdir -p "$out/$tool"; done

i=0
gt_ok=0 q_ok=0 ls_ok=0 op_ok=0 sr_ok=0 ls_found=0
while IFS= read -r sid; do
	i=$((i + 1))
	[ -z "$sid" ] && continue

	# get_trace: the annotations array only, every ts deleted (it is the call time)
	if mcp get_trace "{\"session_id\":\"$sid\",\"include_annotations\":true}" "$TMP/gt.json" &&
		jq '[(.result.structuredContent.annotations // []) | walk(if type == "object" then del(.ts) else . end)]' \
			"$TMP/gt.json" > "$out/get_trace/$i.json" 2>/dev/null; then
		gt_ok=$((gt_ok + 1))
	else
		echo '{"extract_failed":true}' > "$out/get_trace/$i.json"
	fi

	# list window: list_sessions has no id filter; page a +-2 h window around
	# the session's last span end (same data -> same window -> same entry)
	w=$(jq -r '[.. | objects | .End? // empty] | max // ""' "$TMP/gt.json")
	if [ -n "$w" ]; then
		wep=$(date -u -d "$w" +%s)
		start=$(date -u -d "@$((wep - 7200))" +%Y-%m-%dT%H:%M:%SZ)
		end=$(date -u -d "@$((wep + 7200))" +%Y-%m-%dT%H:%M:%SZ)
	else
		start=2000-01-01T00:00:00Z
		end=2100-01-01T00:00:00Z
	fi

	# query: the annotations relation rows for the target session, ts excluded
	# by the select, sorted by annotation_id (the id is content-derived)
	bare=${sid#session:}
	q="from: annotations | filter: target_session_id = '$bare' | select: annotation_id, target_session_id, target_span_id, name, value_number, value_label, value_text, source, author | sort: annotation_id asc"
	if mcp query "{\"query\":\"$q\",\"from\":\"$FROM\",\"to\":\"$TO\",\"limit\":1000}" "$TMP/q.json" &&
		jq '.result.structuredContent // {extract_failed:true}' "$TMP/q.json" > "$out/query/$i.json" 2>/dev/null; then
		q_ok=$((q_ok + 1))
	else
		echo '{"extract_failed":true}' > "$out/query/$i.json"
	fi

	# list_sessions: the session's one entry
	found=0
	cursor=""
	pages=0
	while [ "$pages" -lt 40 ]; do
		body="{\"start_datetime\":\"$start\",\"end_datetime\":\"$end\",\"limit\":25"
		if [ -n "$cursor" ]; then body="$body,\"cursor\":\"$cursor\""; fi
		body="$body}"
		mcp list_sessions "$body" "$TMP/ls.json" || break
		jq -c --arg sid "$sid" '(.result.structuredContent.items // .result.structuredContent.Items // [])[]
			| select((.session_id // .SessionID) == $sid)' "$TMP/ls.json" > "$out/list_sessions/$i.json" 2>/dev/null ||
			: > "$out/list_sessions/$i.json"
		if [ -s "$out/list_sessions/$i.json" ]; then
			found=1
			break
		fi
		cursor=$(jq -r '.result.structuredContent.next_cursor // .result.structuredContent.NextCursor // ""' "$TMP/ls.json" 2>/dev/null || true)
		[ -z "$cursor" -o "$cursor" = null ] && break
		pages=$((pages + 1))
	done
	if [ "$found" = 1 ]; then
		ls_ok=$((ls_ok + 1))
		ls_found=$((ls_found + 1))
	else
		echo '{"found":false}' > "$out/list_sessions/$i.json"
		ls_ok=$((ls_ok + 1))
	fi

	# open: the first page
	if mcp open "{\"id\":\"$sid\",\"limit\":25}" "$TMP/op.json" &&
		jq '.result.structuredContent // {extract_failed:true}' "$TMP/op.json" > "$out/open/$i.json" 2>/dev/null; then
		op_ok=$((op_ok + 1))
	else
		echo '{"extract_failed":true}' > "$out/open/$i.json"
	fi

	# search: the store-wide result ids for "annot-probe", sorted
	if mcp search_sessions '{"query":"annot-probe","n_hits":50}' "$TMP/sr.json" &&
		jq -r '[.result.structuredContent.results[]?.session_id] | unique | .[]' "$TMP/sr.json" > "$out/search/$i.txt" 2>/dev/null; then
		sr_ok=$((sr_ok + 1))
	else
		: > "$out/search/$i.txt"
	fi
done < "$sessions"
echo "$label reads sessions=$i get_trace=$gt_ok query=$q_ok list=$ls_ok (found=$ls_found) open=$op_ok search=$sr_ok"
search_ids=$(cat "$out"/search/*.txt | sort -u | wc -l)
echo "$label search_unique_ids=$search_ids"
grep 'cycle boot' "$log" | tail -1 | sed 's/.*cycle boot: /boot: /' || true
stop_serve

if [ "${RESET:-1}" = 1 ] && [ "$ncalls" -gt 0 ]; then
	for f in "$store"/raw/author=export-annotator/machine=server.mnemonic/harness=annotation/*; do
		[ -f "$f" ] && rm -f "$f"
	done
	log2=$out/reset-serve.log
	start_serve "$log2"
	echo "$label reset boot: $(grep 'cycle boot' "$log2" | sed 's/.*cycle boot: //')"
	echo "$label parts_reset=$(ls "$store/index/parts" | wc -l)"
	stop_serve
fi

(cd "$out" && find get_trace query list_sessions open search -type f | sort | xargs sha256sum) > "$out/SHA256S.txt"
echo "evidence: $out"
