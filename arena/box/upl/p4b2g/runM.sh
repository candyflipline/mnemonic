#!/bin/sh
B=/var/lib/mnu/p4b2g-bin
P=/var/lib/mnu/p4-bin/p4b
D=/var/lib/mnu/p4-store-a
O=/var/lib/mnu/bin/mnemonic-19c8a0e
N=$B/mnemonic-1cdd781
export OUT=/var/lib/mnu/out/p4b2g
date -u +"%T settle start"
PORT=7873 MEM=8G sh $B/hot-settle.sh $N $D | tail -3
date -u +"%T settled"; sh $B/hot-shape.sh $D
date -u +"%T build start"
HOME_KEEP=/var/lib/mnu/laghome-p4b-hot HOT_EVERY=8 HOT_ACTIVE="5 18" HOT_IDLE_SET="0 1 2 4 6 10 20 21 22 23 24 25 26 27 28 29 30 31 32" \
 N=1 TAG=M-build PORT=7875 PUSH_PORT=7876 FIRST_MAX=3600 IDX_MAX=600 \
 SERVE_WRAP="systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0" sh $P/lag.sh $N $D > $OUT/M-build.lag.txt 2>&1
echo "build_rc $?"; grep -E "^(hot_files|warmup|cycle_s_steady|hot_steady|cycle_err|hot_appends)" $OUT/M-build.lag.txt
date -u +"%T built"; sh $B/hot-shape.sh $D
cd $D/index && for i in $(ls deltas); do echo "img ${i%${i#????????}} light $(stat -c %s parts/$i/light) heavy $(stat -c %s parts/$i/heavy)"; done
cd /
sh $B/boot-mem.sh $N $D M-1cdd781-r1
sh $B/boot-mem.sh $O $D M-19c8a0e-r1
sh $B/boot-mem.sh $N $D M-1cdd781-r2
echo RUN-DONE
