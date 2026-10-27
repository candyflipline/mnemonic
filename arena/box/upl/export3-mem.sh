#!/bin/sh
# export3-mem.sh STORE SERVEBIN CLIENTBIN IDS OUT
# Serve memory of the big exports, one serve boot: after an idle settle
# (clear_refs 5, 60 s idle) it measures idle VmHWM, then per run clears refs,
# streams one export through the CLI client (line 1 of IDS is the largest
# session: 3x atif, 1x otlp) or the whole-store dataset (annotation
# export3-none, per-task 1: no session matches; 3x), and records rc, bytes,
# client max RSS, serve VmHWM, its delta over idle and the serve cgroup's
# memory.events. Never prints content. Memory-only numbers and byte identity:
# run under flock -s /var/lib/mnu/box.lock.
set -eu
store=$1 servebin=$2 client=$3 ids=$4 out=$5
port=${PORT:-18872}
url=http://127.0.0.1:$port
mkdir -p "$out"
SERVE= SAMPLER=
cleanup() {
	set +e
	[ -n "$SAMPLER" ] && kill "$SAMPLER" 2>/dev/null
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
systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 env GOMAXPROCS=4 "$servebin" serve --config "$out/serve.toml" > "$out/serve.log" 2>&1 &
SERVE=$!
sleep 1
cg=/sys/fs/cgroup$(cut -d: -f3 "/proc/$SERVE/cgroup")
until grep -q 'cycle boot' "$out/serve.log"; do
	kill -0 "$SERVE" || { echo "serve exited at boot" >&2; exit 1; }
	sleep 2
done
echo "boot: $(grep 'cycle boot' "$out/serve.log" | sed 's/.* in /in /')"
hwm() { awk '/VmHWM/{printf "%.0f", $2/1024}' "/proc/$SERVE/status" 2>/dev/null || echo dead; }
oom() { awk '{for(i=1;i<NF;i++) if($i=="oom_kill") print $(i+1)}' "$cg/memory.events" 2>/dev/null | tr '\n' ' '; }
sleep "${IDLE:-60}"
echo 5 > "/proc/$SERVE/clear_refs"
sleep 5
idle=$(hwm)
echo "serve VmHWM idle MiB=$idle (after clear_refs, 5 s) oom_kill=$(oom)"
(while kill -0 "$SERVE" 2>/dev/null; do awk '/VmRSS/{print $2}' "/proc/$SERVE/status"; sleep 1; done) > "$out/serve-rss.kb" 2>/dev/null &
SAMPLER=$!
hashlen='import hashlib,sys
h=hashlib.sha256();n=0
for b in iter(lambda: sys.stdin.buffer.read(1<<20), b""): h.update(b); n+=len(b)
print(h.hexdigest(), n)'
big=$(sed -n 1p "$ids")
run() { # name kind args...
	name=$1 kind=$2
	shift 2
	echo 5 > "/proc/$SERVE/clear_refs" 2>/dev/null || true
	f=$out/mem.$name
	{ /usr/bin/time -f '%e %M' -o "$f.tm" "$client" export "$kind" "$@" --server "$url" --token-file "$out/reader.tok" --out /dev/stdout 2>"$f.err"; echo $? > "$f.rc"; } | python3 -c "$hashlen" > "$f.sum"
	p=$(hwm)
	echo "$name rc=$(cat "$f.rc") $(cat "$f.sum") client_rss_MiB=$(awk '{printf "%.1f", $2/1024}' "$f.tm") serve_hwm_MiB=$p delta_MiB=$((p - idle)) oom_kill=$(oom)" >> "$out/mem.txt"
	kill -0 "$SERVE" 2>/dev/null || echo "serve died after $name"
}
: > "$out/mem.txt"
for n in 1 2 3; do
	run "atif-largest-$n" atif --session "$big"
done
for n in 1 2 3; do
	run "dataset-all-$n" dataset --annotation export3-none --per-task 1
done
run "otlp-largest-1" otlp --session "$big"
peak=$(sort -n "$out/serve-rss.kb" | tail -1)
echo "serve sampled peak RSS MiB=$((${peak:-0} / 1024)) idle VmHWM MiB=$idle samples=$(wc -l < "$out/serve-rss.kb")"
echo "serve cgroup $(tr '\n' ' ' < "$cg/memory.events" 2>/dev/null || echo gone)"
echo DONE
