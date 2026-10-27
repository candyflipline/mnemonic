#!/bin/sh
# export3-anon.sh STORE SERVEBIN CLIENTBIN IDS OUT
# One boot, idle 60 s (clear_refs 5, 5 s settle), then one largest-atif run
# (line 1 of IDS) and one whole-store dataset run (annotation export3-none,
# per-task 1), the 3b commands. Per phase a sampler reads the serve's status
# RssAnon RssFile RssShmem every 0.5 s (its pid trapped), and the phase
# snapshot records status VmHWM/Rss*, the cgroup's memory.peak, memory.stat
# anon/file and memory.events oom_kill. Echoes per-run peaks and deltas.
# Memory-only numbers: run under flock -s /var/lib/mnu/box.lock.
set -eu
store=$1 servebin=$2 client=$3 ids=$4 out=$5
port=${PORT:-18876}
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

sample() { # name: sample status every 0.5 s until killed
	(while kill -0 "$SERVE" 2>/dev/null; do
		awk '/^(RssAnon|RssFile|RssShmem)/{printf "%s ", $2} END{print ""}' "/proc/$SERVE/status" 2>/dev/null
		sleep 0.5
	done) > "$out/$1.samples" &
	SAMPLER=$!
}
stop() { [ -n "$SAMPLER" ] && kill "$SAMPLER" 2>/dev/null; wait "$SAMPLER" 2>/dev/null || true; SAMPLER=; }
snap() { # name: point values + cgroup counters
	echo "== $1"
	awk '/^(VmHWM|RssAnon|RssFile|RssShmem)/{printf "%s %s ", $1, $2}' "/proc/$SERVE/status" 2>/dev/null; echo
	echo "cgroup.peak $(cat "$cg/memory.peak" 2>/dev/null || echo -)"
	awk '/^(anon|file) /{printf "%s %s ", $1, $2}' "$cg/memory.stat" 2>/dev/null; echo
	echo "oom_kill $(awk '{for(i=1;i<NF;i++) if($i=="oom_kill") print $(i+1)}' "$cg/memory.events" 2>/dev/null)"
}
peaks() { # name idleanon idlefile: per-column peaks and deltas over idle
	f=$out/$1.samples
	a=$(sort -g "$f" | awk 'NF==3{print $1}' | tail -1)
	fl=$(sort -g "$f" | awk 'NF==3{print $2}' | tail -1)
	sh=$(sort -g "$f" | awk 'NF==3{print $3}' | tail -1)
	echo "$1 peak_RssAnon_MiB=$((a / 1024)) delta_anon_MiB=$(((a - $2) / 1024)) peak_RssFile_MiB=$((fl / 1024)) delta_file_MiB=$(((fl - $3) / 1024)) peak_RssShmem_MiB=$((sh / 1024)) samples=$(grep -c . "$f")"
}
big=$(sed -n 1p "$ids")
sample idle
sleep "${IDLE:-60}"
echo 5 > "/proc/$SERVE/clear_refs"
sleep 5
snap idle | tee "$out/idle.txt"
stop
ia=$(awk 'NR==2{print $4}' "$out/idle.txt")
ifl=$(awk 'NR==2{print $6}' "$out/idle.txt")

sample atif
echo 5 > "/proc/$SERVE/clear_refs"
rc=0
"$client" export atif --session "$big" --server "$url" --token-file "$out/reader.tok" --out /dev/null 2>"$out/atif.err" || rc=$?
echo "atif rc=$rc $(head -c 80 "$out/atif.err")"
snap atif-run | tee "$out/atif.txt"
stop
peaks atif "$ia" "$ifl"

sample dataset
echo 5 > "/proc/$SERVE/clear_refs"
rc=0
/usr/bin/time -f '%e' -o "$out/dataset.wall" "$client" export dataset --annotation export3-none --per-task 1 --server "$url" --token-file "$out/reader.tok" --out /dev/null 2>"$out/dataset.err" || rc=$?
echo "dataset rc=$rc wall_s=$(cat "$out/dataset.wall") err=$(head -c 80 "$out/dataset.err")"
snap dataset-run | tee "$out/dataset.txt"
stop
peaks dataset "$ia" "$ifl"
echo "cgroup final $(tr '\n' ' ' < "$cg/memory.events" 2>/dev/null || echo gone)"
echo DONE
