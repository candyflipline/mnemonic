#!/bin/sh
# ingest-timed.sh — the cold full-corpus ingest numbers for one binary.
#
## Inputs
##   $1  BIN  the binary under test speaking the arena contract's
##            `ingest -root RAWROOT -store STORE` (oracle or engine).
## Env: BOX (default /var/lib/mnemonic-e3) — the dataroot whose raw/ is the
##      frozen corpus, and where stores and logs land. RUNS (default 3).
#
# RUNS timed cold ingests, sequential, each into a FRESH empty store
# $BOX/cold-N with the page cache dropped first
# (sync; echo 3 > /proc/sys/vm/drop_caches), GOMAXPROCS=16 and no GOMEMLIMIT,
# under /usr/bin/time -v, with 1 s vmstat (and pidstat when installed)
# sampling alongside into the run's log. One log per run:
# $BOX/out/ingest-cold-N.log (time -v output, the binary's own ingest counts,
# the samplers). Summary lines (tab separated: run wall_s rss_mib user_s
# sys_s cpu_pct sources rewrote failed events conv_files inserted sessions)
# to stdout and $BOX/out/ingest-cold-summary.tsv; the last lines name the
# median wall and RSS and the range.
#
# Stores are left in place on purpose: $BOX/cold-3 is the oracle root the
# gen step uses and $BOX/cold-1 the second store the masks diff needs
# (gen-masks.sh depends on both).
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
RUNS=${RUNS:-3}
BIN=${1:?usage: ingest-timed.sh BIN}
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
DATAROOT=$BOX
OUT=$BOX/out
mkdir -p "$OUT"
: > "$OUT/ingest-cold-summary.tsv"
export GOMAXPROCS=16
unset GOMEMLIMIT || true

for i in $(seq 1 "$RUNS"); do
	STORE=$BOX/cold-$i
	rm -rf "$STORE"
	mkdir -p "$STORE"
	sync
	echo 3 > /proc/sys/vm/drop_caches
	LOG=$OUT/ingest-cold-$i.log
	: > "$LOG"
	if command -v pidstat >/dev/null 2>&1; then
		pidstat -u -d -h 1 2>> "$LOG" &
		PIDS=$!
	fi
	vmstat 1 >> "$LOG" 2>&1 &
	VMS=$!
	set +e
	/usr/bin/time -v "$BIN" ingest -root "$DATAROOT" -store "$STORE" >> "$LOG" 2>&1
	RC=$?
	set -e
	kill "$VMS" 2>/dev/null || true
	[ -z "${PIDS:-}" ] || kill "$PIDS" 2>/dev/null || true
	wait "$VMS" 2>/dev/null || true
	[ -z "${PIDS:-}" ] || wait "$PIDS" 2>/dev/null || true
	if [ "$RC" -ne 0 ]; then
		echo "ingest-timed.sh: run $i exited $RC, see $LOG" >&2
		exit "$RC"
	fi
	python3 -I - "$i" "$LOG" >> "$OUT/ingest-cold-summary.tsv" <<'PYEOF'
import re, sys
run, path = sys.argv[1], sys.argv[2]
t = open(path, encoding="utf-8", errors="replace").read()
m = re.search(r"Elapsed \(wall clock\) time [^:]*: (\S+)", t)
wall = m.group(1) if m else ""
parts = wall.split(":")
secs = 0.0
for p in parts:
    secs = secs * 60 + float(p)
m = re.search(r"Maximum resident set size \(kbytes\): (\d+)", t)
rss = int(m.group(1)) / 1024 if m else 0.0
m = re.search(r"User time \(seconds\): ([\d.]+)", t)
user = float(m.group(1)) if m else 0.0
m = re.search(r"System time \(seconds\): ([\d.]+)", t)
sys_ = float(m.group(1)) if m else 0.0
m = re.search(r"Percent of CPU this job got: (\S+)%", t)
cpu = m.group(1) if m else ""
def count(name):
    m = re.search(r"ingest: sources=(\d+) rewrote=(\d+) failed=(\d+) events=(\d+) converge files=(\d+) inserted=(\d+) sessions=(\d+)", t)
    return m.group(int(name)) if m else ""
print("\t".join([run, "%.1f" % secs, "%.1f" % rss, "%.1f" % user, "%.1f" % sys_, cpu,
                 count(1), count(2), count(3), count(4), count(5), count(6), count(7)]))
PYEOF
done

python3 -I - "$OUT/ingest-cold-summary.tsv" <<'PYEOF'
import sys
rows = [l.split("\t") for l in open(sys.argv[1]).read().splitlines() if l.strip()]
wall = sorted(float(r[1]) for r in rows)
rss = sorted(float(r[2]) for r in rows)
med = lambda xs: xs[len(xs) // 2] if len(xs) % 2 else (xs[len(xs)//2 - 1] + xs[len(xs)//2]) / 2
print("median_wall_s\t%.1f" % med(wall))
print("range_wall_s\t%.1f-%.1f" % (wall[0], wall[-1]))
print("median_rss_mib\t%.1f" % med(rss))
print("range_rss_mib\t%.1f-%.1f" % (rss[0], rss[-1]))
PYEOF
