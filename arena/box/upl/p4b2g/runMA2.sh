#!/bin/sh
B=/var/lib/mnu/p4b2g-bin
P=/var/lib/mnu/p4-bin/p4b
D=/var/lib/mnu/p4-store-a
O=/var/lib/mnu/bin/mnemonic-19c8a0e
N=$B/mnemonic-1cdd781
export OUT=/var/lib/mnu/out/p4b2g
for r in 3 4 5; do
	sh $B/boot-mem.sh $O $D M-19c8a0e-r$r
	sh $B/boot-mem.sh $N $D M-1cdd781-r$r
done
date -u +"%T settle start"
PORT=7873 MEM=8G MAXHOT=3 sh $B/hot-settle.sh $N $D | tail -3
date -u +"%T settled"; sh $B/hot-shape.sh $D
HOME_KEEP=/var/lib/mnu/laghome-p4b-hot HOT_EVERY=8 HOT_ACTIVE="5 6" HOT_IDLE_SET="0 1 2 3 4 7 8 9 10 11 12 13 14 15 16 17 18 19" \
 N=1 TAG=A2-build PORT=7875 PUSH_PORT=7876 FIRST_MAX=3600 IDX_MAX=600 \
 SERVE_WRAP="systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0" sh $P/lag.sh $N $D > $OUT/A2-build.lag.txt 2>&1
echo "build_rc $?"; grep -E "^(hot_files|cycle_s_steady|hot_steady|cycle_err|hot_appends)" $OUT/A2-build.lag.txt
date -u +"%T built"; sh $B/hot-shape.sh $D
sh $B/boot-mem.sh $N $D A2-1cdd781-r0
for r in 1 2 3; do
	sh $B/boot-mem.sh $O $D A2-19c8a0e-r$r
	sh $B/boot-mem.sh $N $D A2-1cdd781-r$r
done
echo RUN-DONE
