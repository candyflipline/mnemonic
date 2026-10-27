#!/bin/sh
# annot-cycle.sh STORE BIN SESSIONS NCALLS LABEL [OUTDIR]
#
# One paired-capture variant for the annotate stall. On a settled full-store
# copy STORE with no annotation source, starts a serve (MemoryMax 8G, as prod),
# makes NCALLS annotate calls (session i mod lines of SESSIONS, name
# annot-probe-i) and records the put cycle(s) they cause: the cycle log line,
# the wall, the part bytes it wrote, VmHWM since the calls began (clear_refs
# reset after boot), the sampled RSS peak, cgroup memory.events, pidstat CPU and
# disk, vmstat. Then it puts STORE back: stops serve, removes the annotation
# source's two raw files (never recursive), boots a second serve and records
# that boot cycle too (the reverse rebuild), so the next variant starts from the
# same state. NCALLS=0 records the control: an idle put-free window.
# SESSIONS holds bare engine session ids; the annotate tool takes
# "session:" + base64url of that id, derived per call here (the bare form is
# refused: parseMcpID requires the prefix, internal/index/q_common.go:66).
# Run under `flock -x /var/lib/mnu/box.lock` (timed). Prints counts and
# timings only; session ids are read from SESSIONS and never printed.
set -eu
store=${1:?usage: annot-cycle.sh STORE BIN SESSIONS NCALLS LABEL [OUTDIR]}
bin=${2:?BIN}
sessions=${3:?SESSIONS file, one id per line}
ncalls=${4:?NCALLS}
label=${5:?LABEL}
out=${6:-/var/lib/mnu/annot-ev/$label}
port=${PORT:-18860}
url=http://127.0.0.1:$port
mkdir -p "$out"
TMP=$(mktemp -d /var/lib/mnu/annot-run.XXXXXX)
SERVE= SAMPLER= PIDSTAT= VMSTAT= SAMP2=
stop_serve() {
	set +e
	for p in $PIDSTAT $SAMPLER $VMSTAT $SAMP2; do kill "$p" 2>/dev/null; done
	PIDSTAT= SAMPLER= VMSTAT= SAMP2=
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

head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$TMP/admin.tok"
hash=$(printf %s "$(cat "$TMP/admin.tok")" | sha256sum | cut -d' ' -f1)
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
	cg=/sys/fs/cgroup$(cut -d: -f3 "/proc/$SERVE/cgroup")
	until grep -q 'cycle boot' "$1"; do
		kill -0 "$SERVE" 2>/dev/null || { echo "serve exited at boot" >&2; cat "$1" >&2; exit 1; }
		sleep 2
	done
}
hwm() { awk '/VmHWM/{printf "%.0f", $2/1024}' "/proc/$SERVE/status"; }
pcount() { grep -c 'cycle put' "$1" || true; }

log=$out/serve.log
start_serve "$log"
echo "$label boot: $(grep 'cycle boot' "$log" | sed 's/.*cycle boot: //')"
echo "$label parts_before=$(ls "$store/index/parts" | wc -l) bytes_before=$(du -sb "$store/index/parts" | cut -f1)"
marker=$TMP/marker
touch "$marker"
sleep 2
echo 5 > "/proc/$SERVE/clear_refs"
(while kill -0 "$SERVE" 2>/dev/null; do
	awk '/VmRSS/{print $2}' "/proc/$SERVE/status"
	sleep 1
done) > "$out/rss.kb" &
SAMPLER=$!
(while kill -0 "$SERVE" 2>/dev/null; do
	cat "$cg/memory.current"
	sleep 1
done) > "$out/cgcur.b" &
SAMP2=$!
pidstat -u -d -p "$SERVE" 5 > "$out/pidstat.log" 2>&1 &
PIDSTAT=$!
vmstat 5 > "$out/vmstat.log" 2>&1 &
VMSTAT=$!

before=$(pcount "$log")
nlines=$(wc -l < "$sessions")
t0=$(date +%s)
ok=0 fail=0 dup=0
i=0
while [ "$i" -lt "$ncalls" ]; do
	i=$((i + 1))
	sid=$(sed -n "$(((i - 1) % nlines + 1))p" "$sessions")
	pref=$(printf 'session:%s' "$(printf %s "$sid" | base64 -w0 | tr '+/' '-_' | tr -d '=')")
	r=$(curl -sf -H 'MCP-Protocol-Version: 2025-06-18' -H "Authorization: Bearer $(cat "$TMP/admin.tok")" \
		-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
		-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"annotate\",\"arguments\":{\"session_id\":\"$pref\",\"name\":\"annot-probe-$i\",\"number\":$i}}}" "$url/mcp" || echo '{}')
	e=$(printf %s "$r" | jq -r '.result.isError // false')
	d=$(printf %s "$r" | jq -r '.result.structuredContent.data.duplicate // false')
	if [ "$e" = false ]; then ok=$((ok + 1)); else fail=$((fail + 1)); fi
	[ "$d" = true ] && dup=$((dup + 1))
done
t1=$(date +%s)
echo "$label annotate calls=$ncalls ok=$ok failed=$fail duplicate=$dup over=$((t1 - t0))s"

if [ "$ncalls" -gt 0 ]; then
	waited=0
	while [ "$(pcount "$log")" = "$before" ] && [ "$waited" -lt 900 ]; do
		sleep 1
		waited=$((waited + 1))
	done
	# a second put cycle would start within settleMax (3 s) of the last call;
	# wait past that so "one rebuild or N" is read from the log
	sleep 20
fi
sleep 5
echo "$label put_cycles=$(($(pcount "$log") - before))"
grep 'cycle put' "$log" | tail -n +$((before + 1)) | sed 's/.*cycle put: /put: /'
echo "$label VmHWM_MiB_since_calls=$(hwm) sampled_peak_RSS_MiB=$(sort -n "$out/rss.kb" | tail -1 | awk '{printf "%.0f", $1/1024}') cgroup_current_peak_MiB=$(sort -n "$out/cgcur.b" | tail -1 | awk '{printf "%.0f", $1/1048576}')"
echo "$label cgroup $(tr '\n' ' ' < "$cg/memory.events")"
echo "$label parts_after=$(ls "$store/index/parts" | wc -l) new_part_dirs=$(find "$store/index/parts" -maxdepth 1 -mindepth 1 -type d -newer "$marker" | wc -l) bytes_after=$(du -sb "$store/index/parts" | cut -f1)"
kill -INT "$PIDSTAT" 2>/dev/null || true
sleep 1
grep Average "$out/pidstat.log" | sed "s/^/$label pidstat /" || true
awk 'NR>2 && $0 ~ /^ *[0-9]/ {w+=$16; n++; if($16>mx)mx=$16} END{if(n) printf "'"$label"' vmstat samples=%d avg_iowait=%.1f max_iowait=%d\n", n, w/n, mx}' "$out/vmstat.log"
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
echo "evidence: $out"
