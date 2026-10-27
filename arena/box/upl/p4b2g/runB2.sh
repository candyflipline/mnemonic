#!/bin/sh
B=/var/lib/mnu/p4b2g-bin
D=/var/lib/mnu/p4-store-a
O=/var/lib/mnu/bin/mnemonic-19c8a0e
N=$B/mnemonic-1cdd781
sh $B/boot-mem.sh $N $D B2-prod-1cdd781-r1
sh $B/boot-mem.sh $O $D B2-prod-19c8a0e-r1
sh $B/boot-mem.sh $N $D B2-prod-1cdd781-r2
sh $B/boot-mem.sh $O $D B2-prod-19c8a0e-r2
echo RUN-DONE
