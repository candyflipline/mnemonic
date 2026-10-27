#!/bin/sh
# export3-route.sh STORE SERVEBIN CLIENTBIN IDS OUT REFDIRS [LINES]
# Serves STORE (MemoryMax=8G, GOMAXPROCS=4, as prod) and asks GET
# /v1/export/{atif,otlp} through the CLI client for each id (only the line
# numbers in LINES when given), hashing the body as it streams, and compares
# with the first old.<kind>.<n>.sum found in REFDIRS (space-separated).
# Records serve VmHWM after an idle settle (clear_refs 5 resets it) and after
# each export, and the serve cgroup's memory.events. Never prints content.
# Memory-only numbers and byte identity: run under flock -s /var/lib/mnu/box.lock.
set -eu
store=$1 servebin=$2 client=$3 ids=$4 out=$5
refdirs=$6
lines=${7:-}
port=${PORT:-18870}
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
sleep "${IDLE:-60}"
echo 5 > "/proc/$SERVE/clear_refs"
sleep 5
idle=$(hwm)
echo "serve VmHWM idle MiB=$idle (after clear_refs, 5 s)"
(while kill -0 "$SERVE" 2>/dev/null; do awk '/VmRSS/{print $2}' "/proc/$SERVE/status"; sleep 1; done) > "$out/serve-rss.kb" 2>/dev/null &
SAMPLER=$!
hashlen='import hashlib,sys
h=hashlib.sha256();n=0
for b in iter(lambda: sys.stdin.buffer.read(1<<20), b""): h.update(b); n+=len(b)
print(h.hexdigest(), n)'
refsum() { # kind i: the ref sum file, if any
	for d in $refdirs; do
		[ -f "$d/old.$1.$2.sum" ] && { echo "$d/old.$1.$2.sum"; return; }
	done
}
same=0 differ=0 fail=0 i=0
: > "$out/runs.txt"
while read -r sid; do
	i=$((i + 1))
	if [ -n "$lines" ]; then case " $lines " in *" $i "*) ;; *) continue ;; esac; fi
	for kind in ${KINDS:-atif otlp}; do
		echo 5 > "/proc/$SERVE/clear_refs" 2>/dev/null || true
		f=$out/new.$kind.$i
		{ /usr/bin/time -f '%e %M %P' -o "$f.tm" "$client" export "$kind" --session "$sid" --server "$url" --token-file "$out/reader.tok" --out /dev/stdout 2>"$f.err"; echo $? > "$f.rc"; } | python3 -c "$hashlen" > "$f.sum"
		peak=$(hwm)
		echo "$kind.$i rc=$(cat "$f.rc") $(cat "$f.sum") client=$(tail -1 "$f.tm") serve_hwm_MiB=$peak" >> "$out/runs.txt"
		rs=$(refsum "$kind" "$i")
		if [ "$(cat "$f.rc")" != 0 ]; then
			fail=$((fail + 1))
			echo "failed: $kind.$i rc=$(cat "$f.rc") $(head -c 200 "$f.err")"
		elif [ -n "$rs" ] && [ "$(cut -d' ' -f1 "$f.sum")" = "$(cut -d' ' -f1 "$rs")" ]; then
			same=$((same + 1))
		else
			differ=$((differ + 1))
			echo "differ: $kind.$i"
		fi
		kill -0 "$SERVE" 2>/dev/null || { echo "serve died after $kind.$i"; break 2; }
	done
done < "$ids"
echo "atif+otlp: compared=$((same + differ + fail)) same=$same differ=$differ failed=$fail"
peak=$(sort -n "$out/serve-rss.kb" | tail -1)
echo "serve sampled peak RSS MiB=$((${peak:-0} / 1024)) idle VmHWM MiB=$idle samples=$(wc -l < "$out/serve-rss.kb")"
echo "serve cgroup $(tr '\n' ' ' < "$cg/memory.events" 2>/dev/null || echo gone)"
echo DONE
