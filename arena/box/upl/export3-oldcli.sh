#!/bin/sh
set -eu
store=$1 servebin=$2 oldbin=$3 out=$4
port=${PORT:-18874}
url=http://127.0.0.1:$port
mkdir -p "$out"
SERVE=
cleanup() {
	set +e
	if [ -n "$SERVE" ]; then
		kill "$SERVE" 2>/dev/null
		for _ in $(seq 1 30); do kill -0 "$SERVE" 2>/dev/null || break; sleep 1; done
		kill -9 "$SERVE" 2>/dev/null
	fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$out/reader3.tok"
cat > "$out/serve3.toml" <<EOT
data_dir = "$store"
listen = "127.0.0.1:$port"
push_listen = "127.0.0.1:$((port + 1))"
[[tokens]]
author = "export3-probe"
sha256 = "$(printf %s "$(cat "$out/reader3.tok")" | sha256sum | cut -d' ' -f1)"
scopes = ["read"]
EOT
serves=$(pgrep -c -f ' serve --config ' || true)
avail=$(free -g | awk '/^Mem:/{print $7}')
if [ "$serves" -ge 3 ] || [ "$avail" -lt 10 ]; then
	echo "refusing to start serve: $serves serves running, ${avail} GiB available" >&2
	exit 1
fi
systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 env GOMAXPROCS=4 "$servebin" serve --config "$out/serve3.toml" > "$out/serve3.log" 2>&1 &
SERVE=$!
sleep 1
until grep -q 'cycle boot' "$out/serve3.log" 2>/dev/null; do
	kill -0 "$SERVE" || { echo "serve exited at boot" >&2; exit 1; }
	sleep 2
done
sleep 5
echo "serve up (writer lock held), boot: $(grep 'cycle boot' "$out/serve3.log" | sed 's/.* in /in /')"
for i in $(seq 1 20); do
	avail=$(free -g | awk '/^Mem:/{print $7}')
	[ "$avail" -ge 24 ] && break
	echo "waiting for 24 GiB available (have ${avail}), try $i"
	sleep 60
done
if [ "$avail" -lt 24 ]; then
	echo "SKIP 3d: available stayed at ${avail} GiB"
	exit 0
fi
systemd-run --scope -q -p MemoryMax=26G -p MemorySwapMax=0 env GOMAXPROCS=4 \
	/usr/bin/time -v -o "$out/old-dataset.time" \
	"$oldbin" export dataset --annotation export3-none --per-task 1 \
	--data-dir "$store" --out "$out/old-dataset.jsonl" > "$out/old-dataset.err" 2>&1 \
	|| echo "old rc=$?"
echo "old rc=$(cat "$out/old-dataset.err" >/dev/null 2>&1; grep -o 'Exit status: [0-9]*' "$out/old-dataset.time" || echo '?')"
grep 'Maximum resident set size' "$out/old-dataset.time" || true
[ -f "$out/old-dataset.jsonl" ] && echo "old bytes=$(stat -c %s "$out/old-dataset.jsonl") sha=$(sha256sum < "$out/old-dataset.jsonl" | cut -d' ' -f1)" || echo "old bytes=0 sha=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 (no file)"
echo "old stderr lines=$(wc -l < "$out/old-dataset.err") skipped=$(grep -c 'skipped session' "$out/old-dataset.err" || true)"
echo DONE
