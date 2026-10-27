#!/bin/sh
set -eu
SRC=${1:?usage: memcap-open.sh SRC OUT TAG}
OUT=${2:?usage: memcap-open.sh SRC OUT TAG}
TAG=${3:?usage: memcap-open.sh SRC OUT TAG}
MEM=${MEM:-8G}
mkdir -p "$OUT"
export MEMCAP_STORE=${MEMCAP_STORE:-/var/lib/mnu/p4-store-a/index}
export MEMCAP_HEAP=$OUT/heap-$TAG.pprof
export MEMCAP_SECHASH=${MEMCAP_SECHASH:-}
cd "$SRC"
/usr/local/go/bin/go test -c -o "$SRC/memcap.test" ./internal/index
echo "bin_sha256 $(sha256sum "$SRC/memcap.test" | cut -c1-12) mem $MEM free_avail_gb $(free -g | awk '/^Mem:/{print $7}') serves $(pgrep -c -f ' serve --config ' || true)"
t0=$(date +%s)
UNIT=memcap-$TAG-$t0
systemd-run --scope -q --unit=$UNIT -p MemoryMax=$MEM -p MemorySwapMax=0 \
	env GOMAXPROCS=4 "$SRC/memcap.test" -test.run TestScratchMemcap -test.v \
	-test.memprofile "$OUT/alloc-$TAG.pprof" >"$OUT/memcap-$TAG.log" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
peak=0 anon=0
while kill -0 $PID 2>/dev/null; do
	CG=/sys/fs/cgroup/system.slice/$UNIT.scope
	p=$(cat "$CG/memory.peak" 2>/dev/null || echo 0)
	a=$(awk '$1=="anon"{print $2}' "$CG/memory.stat" 2>/dev/null || echo 0)
	[ "${p:-0}" -gt "$peak" ] && peak=$p
	[ "${a:-0}" -gt "$anon" ] && anon=$a
	sleep 1
done
wait "$PID" || rc=$?
rc=${rc:-0}
trap - EXIT
echo "memory_peak_bytes $peak anon_max_bytes $anon test_rc $rc"
grep -E 'MEMCAP_|^--- |^(ok|FAIL|PASS)' "$OUT/memcap-$TAG.log" | head -90
