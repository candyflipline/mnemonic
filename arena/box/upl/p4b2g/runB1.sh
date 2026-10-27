#!/bin/sh
B=/var/lib/mnu/p4b2g-bin
D=/var/lib/mnu/p4-store-a
sh $B/boot-mem.sh $B/mnemonic-1cdd781 $D B1-prod-1cdd781
sh $B/boot-mem.sh /var/lib/mnu/bin/mnemonic-19c8a0e $D B1-prod-19c8a0e
echo RUN-DONE
