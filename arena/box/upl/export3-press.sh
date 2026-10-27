#!/bin/sh
# export3-press.sh STORE SERVEBIN CLIENTBIN IDS OUT REFSUM
# One boot, serve scope named export3-press (MemoryMax=8G until tightened),
# idle 60 s, then the scope's live MemoryMax drops to idle-anon + 512 MiB
# (systemctl set-property --runtime; memory.max echoed back). Then the 3b
# commands in order: largest atif (line 1 of IDS) and the whole-store dataset
# (annotation export3-none, per-task 1). During each, a loop calls MCP
# list_sessions (limit 5) every 2 s, recording HTTP status and latency per
# call; its pid is trapped. Per run: rc, output sha256+bytes (atif vs REFSUM,
# dataset vs the empty sha), memory.events before/after, peak RssAnon, cgroup
# memory.peak, wall, list_sessions ok/failed/max-latency. If the serve dies,
# the script stops, dumps memory.events and the last samples, and never raises
# the limit. Memory-only numbers and byte identity: run under flock -s
# /var/lib/mnu/box.lock.
set -eu
store=$1 servebin=$2 client=$3 ids=$4 out=$5 refsum=$6
port=${PORT:-18878}
url=http://127.0.0.1:$port
emptysha=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
mkdir -p "$out"
SERVE= SAMPLER= LOOP=
cleanup() {
	set +e
	[ -n "$SAMPLER" ] && kill "$SAMPLER" 2>/dev/null
	[ -n "$LOOP" ] && kill "$LOOP" 2>/dev/null
	if [ -n "$SERVE" ]; then
		kill "$SERVE" 2>/dev/null
		for _ in $(seq 1 30); do kill -0 "$SERVE" 2>/dev/null || break; sleep 1; done
		kill -9 "$SERVE" 2>/dev/null
	fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$out/reader.tok"
cat > "$out/serve.toml" <<EOT
data_dir = "$store"
listen = "127.0.0.1:$port"
push_listen = "127.0.0.1:$((port + 1))"
[[tokens]]
author = "export3-probe"
sha256 = "$(printf %s "$(cat "$out/reader.tok")" | sha256sum | cut -d' ' -f1)"
scopes = ["read"]
EOT
serves=$(pgrep -c -f ' serve --config ' || true)
avail=$(free -g | awk '/^Mem:/{print $7}')
if [ "$serves" -ge 3 ] || [ "$avail" -lt 10 ]; then
	echo "refusing to start serve: $serves serves running, ${avail} GiB available" >&2
	exit 1
fi
systemd-run --scope -q --unit=export3-press -p MemoryMax=8G -p MemorySwapMax=0 \
	env GOMAXPROCS=4 "$servebin" serve --config "$out/serve.toml" > "$out/serve.log" 2>&1 &
SERVE=$!
sleep 1
cg=/sys/fs/cgroup$(cut -d: -f3 "/proc/$SERVE/cgroup")
until grep -q 'cycle boot' "$out/serve.log"; do
	kill -0 "$SERVE" || { echo "serve exited at boot" >&2; exit 1; }
	sleep 2
done
echo "boot: $(grep 'cycle boot' "$out/serve.log" | sed 's/.* in /in /')"

sample() { # name: RssAnon RssFile RssShmem every 0.5 s until killed
	(while kill -0 "$SERVE" 2>/dev/null; do
		awk '/^(RssAnon|RssFile|RssShmem)/{printf "%s ", $2} END{print ""}' "/proc/$SERVE/status" 2>/dev/null
		sleep 0.5
	done) > "$out/$1.samples" &
	SAMPLER=$!
}
stop() { for p in "$SAMPLER" "$LOOP"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done; wait "$SAMPLER" 2>/dev/null || true; SAMPLER=; LOOP=; }
events() { # label: cgroup memory.peak + full memory.events counters
	echo "events.$1 peak=$(cat "$cg/memory.peak" 2>/dev/null || echo -) max=$(cat "$cg/memory.max" 2>/dev/null || echo -) $(tr '\n' ' ' < "$cg/memory.events" 2>/dev/null || echo gone)"
}
mcpl() { # name: list_sessions limit 5 every 2 s, "status latency" per call
	(while kill -0 "$SERVE" 2>/dev/null; do
		curl -s -o /dev/null -w '%{http_code} %{time_total}\n' --max-time 30 \
			-X POST "$url/mcp" \
			-H "Authorization: Bearer $(cat "$out/reader.tok")" \
			-H 'Content-Type: application/json' -H 'Accept: application/json' \
			-H 'Mcp-Protocol-Version: 2025-06-18' \
			-d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{"limit":5}}}'
		sleep 2
	done) > "$out/$1.mcp" &
	LOOP=$!
}
mcpsum() { # name: ok/failed/max latency over the recorded calls
	awk 'NF==2{t+=$2; n++; if($2>m)m=$2; if($1==200)ok++; else bad++} END{d=0; if(n>0)d=t/n; printf "ok=%d failed=%d max_lat=%.3fs mean_lat=%.3fs", ok+0, bad+0, m+0, d}' "$out/$1.mcp"
}
peaks() { # name: per-column sample peaks
	f=$out/$1.samples
	awk 'NF==3{n++; if($1+0>a)a=$1+0; if($2+0>f)f=$2+0}END{printf "peak_RssAnon_MiB=%d peak_RssFile_MiB=%d samples=%d", a/1024, f/1024, n+0}' "$f"
}
died() { # last words when the serve is gone: events + last samples, then stop
	echo "SERVE DIED during $1; not raising the limit"
	events "$1-dead"
	echo "last samples: $(tail -3 "$out/$1.samples" | tr '\n' '|')"
	exit 1
}
hashlen='import hashlib,sys
h=hashlib.sha256();n=0
for b in iter(lambda: sys.stdin.buffer.read(1<<20), b""): h.update(b); n+=len(b)
print(h.hexdigest(), n)'

sample idle
sleep "${IDLE:-60}"
echo 5 > "/proc/$SERVE/clear_refs"
sleep 5
aidle=$(awk '/^anon /{print $2}' "$cg/memory.stat")
limit=$((aidle + 512 * 1024 * 1024))
systemctl set-property --runtime export3-press.scope MemoryMax=$limit
echo "idle anon A_idle=${aidle}B ($((aidle / 1024 / 1024)) MiB); MemoryMax set to ${limit}B ($((limit / 1024 / 1024)) MiB); memory.max=$(cat "$cg/memory.max")"
events idle
stop

big=$(sed -n 1p "$ids")
sample atif
mcpl atif
echo 5 > "/proc/$SERVE/clear_refs" 2>/dev/null || true
rc=0
/usr/bin/time -f '%e' -o "$out/atif.wall" \
	"$client" export atif --session "$big" --server "$url" --token-file "$out/reader.tok" --out "$out/atif.bin" 2>"$out/atif.err" || rc=$?
python3 -c "$hashlen" < "$out/atif.bin" > "$out/atif.sum"
echo "atif rc=$rc wall_s=$(cat "$out/atif.wall") out=$(cat "$out/atif.sum") ref=$(cat "$refsum" | cut -d' ' -f1)"
kill -0 "$SERVE" 2>/dev/null || died atif
stop
events atif
peaks atif
mcpsum atif

sample dataset
mcpl dataset
echo 5 > "/proc/$SERVE/clear_refs" 2>/dev/null || true
rc=0
/usr/bin/time -f '%e' -o "$out/dataset.wall" \
	"$client" export dataset --annotation export3-none --per-task 1 --server "$url" --token-file "$out/reader.tok" --out "$out/dataset.bin" 2>"$out/dataset.err" || rc=$?
python3 -c "$hashlen" < "$out/dataset.bin" > "$out/dataset.sum"
echo "dataset rc=$rc wall_s=$(cat "$out/dataset.wall") out=$(cat "$out/dataset.sum") ref=$emptysha"
kill -0 "$SERVE" 2>/dev/null || died dataset
stop
events dataset
peaks dataset
mcpsum dataset
echo DONE
