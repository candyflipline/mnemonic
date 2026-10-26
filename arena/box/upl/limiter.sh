#!/bin/sh
# limiter.sh PREFIX — the limiter evidence of a push-run or lag run, from the
# pidstat -u -d and vmstat 1 files it wrote ($OUT/base-<name>-pidstat.txt and
# -vmstat.txt; lag runs: base-<name>-lag-*). PREFIX is the path before
# "-pidstat.txt". Per process: mean and max %CPU (100 = one core), mean %usr,
# %system, kB read and written per second, samples. Whole box: mean us/sy/id/wa.
# A process pinned near 100 x GOMAXPROCS is CPU-limited by that setting; the
# box idle column says whether anything else was short. Read only, no lock.
set -eu
P=${1:?usage: limiter.sh PREFIX}
awk '/^#/ || NF < 15 || $0 ~ /^Linux/ {next}
	{ pid=$4; c[pid]+=$9; u[pid]+=$5; s[pid]+=$6; rd[pid]+=$11; wr[pid]+=$12; n[pid]++; if ($9+0 > mx[pid]) mx[pid]=$9+0; cmd[pid]=$15 }
	END { for (p in c) printf "pid %s %s\tmean_cpu_pct %.0f\tmax_cpu_pct %.0f\tusr %.0f\tsys %.0f\tkB_rd_s %.0f\tkB_wr_s %.0f\tsamples %d\n", p, cmd[p], c[p]/n[p], mx[p], u[p]/n[p], s[p]/n[p], rd[p]/n[p], wr[p]/n[p], n[p] }' "$P-pidstat.txt" | sort
awk 'NR > 2 && NF >= 17 { u+=$13; s+=$14; i+=$15; w+=$16; n++ }
	END { if (n) printf "box_vmstat\tus %.1f\tsy %.1f\tid %.1f\twa %.1f\tsamples %d\n", u/n, s/n, i/n, w/n, n }' "$P-vmstat.txt"
