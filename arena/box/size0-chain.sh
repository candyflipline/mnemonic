#!/bin/sh
# size0-chain.sh — lane S0's box run, one lock acquisition across all three:
#   (a) frames census over arch-diag/store/src (read-only, archived first)
#   (c) one PERF C-case append round, MODE=phases: per-source outcomes incl.
#       which reparses a Provisional checkpoint caused
#   (d) cold raw+archive build at GOMAXPROCS=4 under MemoryMax=8G with heap
#       snapshots every 10 s, du + df every 10 s, capped at 2700 s
# Counts, sizes and timings only. Launched detached:
#   nohup flock /var/lib/mnemonic-e3/box.lock sh arena/box/size0-chain.sh \
#     > /dev/null 2>&1 &
set -u
BOX=/var/lib/mnemonic-e3
OUT=$BOX/out
SRC=$BOX/arch-diag/store/src
{
	echo "=== (a) frames census $(date -u +%FT%TZ)"
	/root/size0-census frames "$SRC" > "$OUT/size0-frames.txt" 2>&1
	echo "frames rc=$? lines=$(wc -l < "$OUT/size0-frames.txt")"
	sync
	echo "=== (c) C-case round $(date -u +%FT%TZ)"
	mkdir -p "$OUT/size0-c"
	sh /root/perf/round.sh C /root/perf/index-size0.test phases 4 "$OUT/size0-c"
	echo "round rc=$?"
	echo "=== (d) cold build $(date -u +%FT%TZ)"
	FREE=$(df --output=avail -B1 "$BOX" | tail -1 | tr -d " ")
	echo "free_before_d=$FREE"
	# Foreman: skip the cold build under 70 GB free (ARCH2's oracle-clean is
	# still growing) and report it as pending disk.
	if [ "$FREE" -lt 70000000000 ]; then
		echo "skip (d): free < 70 GB, cold build pending disk"
		exit 0
	fi
	# Never run past the lane's report window: cap at 07:22Z, with an
	# 8-minute floor (a shorter build's profiles are not worth the lock).
	CAP=$(( $(date -u -d 2026-10-25T07:22:00Z +%s) - $(date -u +%s) ))
	echo "cap_seconds=$CAP"
	if [ "$CAP" -lt 480 ]; then
		echo "skip (d): less than 8 min left in the lane window"
		exit 0
	fi
	timeout "$CAP" sh /root/size0-cold.sh /root/perf/index-size0.test
	echo "cold rc=$? (size0-cold.sh = arena/box/size0-cold.sh shipped to /root)"
} > "$OUT/size0-chain.log" 2>&1
