#!/bin/sh
# perf-p2.sh BIN DIR — CPU of a mixed-component append cycle in a running serve.
#
# The engine run's case M (one raw source sharing a session id with archive ch
# members, 10 lines appended, one cycle) started a fresh process per round, so
# each round paid archiveSetFor's first-call scan of every ch file. serve is one
# long-lived process. This measures that shape: one `BIN serve` on DIR, its boot
# cycle, then N rounds of one 10-line PUT append to the mixed target and the
# "cycle put" that indexes it.
#
# Prep, once per DIR (idempotent, outside the timed part):
#   - DIR: an overlayfs view of SRC (default $BOX/http-1, a raw-only store, never
#     written: writes land in DIR.up); the prep stops if / drops under 60 GB free
#   - DIR/events: a symlink tree to the ch files under $BOX/events (read-only)
#     smaller than CH_MAX bytes (default all 16)
#   - one `BIN index ingest -settle` brings the archive in and settles every
#     source, so archive member logs are retired as on prod
#   - the target: the first raw (kind 0) checkpoint, harness claude-code, whose
#     sessions meet an archive member's; a token for its author
# Run: serve at GOMAXPROCS=4 under MemoryMax=8G (prod's shape), pidstat 1 s
# alongside. Per round: SPACING s idle (its CPU is printed), read the target's
# manifest, PUT 10 synthetic claude-code lines at offset=size, wait for the next
# cycle line; cycle CPU = serve utime+stime from just before the PUT to that
# line. Work check per round: PUT 200, the cycle counts resumed+fresh+reparsed
# >= 1, err=<nil>. Prints one TSV row per round, then n, median and range.
# PERF=1 wraps the rounds in `perf record -g` (an unreported profile run) and
# prints the share of samples under archiveSetFor and buildPart.
# Counts, sizes and timings only. Run under the box lock:
#   flock /var/lib/mnu/box.lock sh perf-p2.sh BIN DIR
# Env: BOX (/var/lib/mnu), SRC ($BOX/http-1), OUT ($BOX/out), N (6),
#      SPACING (20), PORT (7795), PERF (0), CH_MAX (all).
set -eu

BIN=${1:?usage: perf-p2.sh BIN DIR}
DIR=${2:?usage: perf-p2.sh BIN DIR}
BOX=${BOX:-/var/lib/mnu}
SRC=${SRC:-$BOX/http-1}
OUT=${OUT:-$BOX/out}
N=${N:-6}
SPACING=${SPACING:-20}
PORT=${PORT:-7795}
PERF=${PERF:-0}
CH_MAX=${CH_MAX:-1000000000000}
SHA8=$(sha256sum "$BIN" | cut -c1-8)
P=$OUT/p2-$SHA8-$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$OUT"

avail=$(awk '/MemAvailable/ {print int($2/1048576)}' /proc/meminfo)
[ "$avail" -ge 8 ] || { echo "MemAvailable ${avail} GB < 8 GB, refusing" >&2; exit 2; }

freegb() { df --output=avail -B1G / | tail -1 | tr -d ' '; }
if ! mountpoint -q "$DIR"; then
	mkdir -p "$DIR" "$DIR.up" "$DIR.work"
	mount -t overlay overlay -o "lowerdir=$SRC,upperdir=$DIR.up,workdir=$DIR.work" "$DIR"
fi
if [ ! -d "$DIR/events" ]; then
	mkdir "$DIR/events"
	cp "$BOX/events/.parser-version" "$DIR/events/" 2>/dev/null || true
	find "$BOX/events" -name 'ch-*.parquet' -size -"$CH_MAX"c | while IFS= read -r f; do
		rel=${f#"$BOX/events/"}
		mkdir -p "$DIR/events/$(dirname "$rel")"
		ln -s "$f" "$DIR/events/$rel"
	done
fi
CHB=$(find -L "$DIR/events" -name 'ch-*.parquet' -printf '%s\n' | awk '{s+=$1} END {print s+0}')
echo "ch files: $(find "$DIR/events" -name 'ch-*.parquet' | wc -l) of $(find "$BOX/events" -name 'ch-*.parquet' | wc -l), bytes $CHB of $(find "$BOX/events" -name 'ch-*.parquet' -printf '%s\n' | awk '{s+=$1} END {print s+0}')"
if [ ! -e "$DIR/.p2-settled" ]; then
	need=$((60 + 2 * CHB / 1000000000 + 4))
	[ "$(freegb)" -ge "$need" ] || { echo "df: $(freegb) GB free < $need (60 + twice the ch bytes + 4)" >&2; exit 2; }
	/usr/bin/time -v systemd-run --scope --quiet -p MemoryMax=8G env GOMAXPROCS=4 \
		"$BIN" index ingest -root "$DIR" -store "$DIR/index" -settle > "$P-prep.log" 2>&1 &
	PREP=$!
	while kill -0 "$PREP" 2>/dev/null; do
		echo "$(date -u +%T) free_gb $(freegb) upper_gb $(du -s -B1G "$DIR.up" | cut -f1)" >> "$P-prep-df.log"
		if [ "$(freegb)" -lt 60 ]; then
			pkill -INT -P "$PREP" 2>/dev/null || true
			echo "DISK GUARD: $(freegb) GB free < 60, prep stopped" >&2
			exit 2
		fi
		sleep 10
	done
	wait "$PREP"
	grep -E '^ingest:|Elapsed .wall|Maximum resident|Percent of CPU' "$P-prep.log"
	touch "$DIR/.p2-settled"
fi

CFG=$DIR/.p2-cfg
if [ ! -e "$CFG/server.toml" ]; then
	mkdir -p "$CFG"
	chmod 700 "$CFG"
	python3 -I - "$DIR/index/src" "$CFG" <<'PY'
import json, os, sys
src, cfg = sys.argv[1], sys.argv[2]
arch, raw = set(), []
for f in sorted(os.listdir(src)):
    if not f.endswith(".ckpt"):
        continue
    c = json.load(open(os.path.join(src, f)))
    if c.get("kind") == 2:
        arch.update(c.get("sessions") or [])
    elif c.get("kind") == 0 and c.get("harness") == "claude-code":
        raw.append(c)
cands = [c for c in raw if arch.intersection(c.get("sessions") or [])]
assert cands, "no raw claude-code source shares a session with an archive member"
c = cands[0]
sid = sorted(arch.intersection(c["sessions"]))[0]
a, m, h, s = c["key"].split("__", 3)
open(cfg + "/target", "w").write("%s %s %s %s %s\n" % (a, m, h, s, sid))
print("mixed raw candidates=%d archive sessions=%d picked rows=%d" % (len(cands), len(arch), c.get("rows", 0)))
PY
	read -r AUTH _ < "$CFG/target"
	"$BIN" token new --author "$AUTH" > "$CFG/token.out"
	{
		echo "data_dir = \"$DIR\""
		echo "listen = \"127.0.0.1:$PORT\""
		tail -n +2 "$CFG/token.out"
	} > "$CFG/server.toml"
fi

python3 -u -I - "$BIN" "$DIR" "$CFG" "$P" "$N" "$SPACING" "$PORT" "$PERF" <<'PY'
import json, os, queue, re, statistics, subprocess, sys, threading, time, urllib.request, uuid
BIN, DIR, CFG, P, N, SPACING, PORT, PERF = sys.argv[1:9]
N, SPACING, PERF = int(N), int(SPACING), PERF == "1"
author, machine, harness, src, sid = open(CFG + "/target").read().split()
token = open(CFG + "/token.out").readline().strip()
man_path = "%s/raw/author=%s/machine=%s/harness=%s/%s.json" % (DIR, author, machine, harness, src)
TCK = os.sysconf("SC_CLK_TCK")
CYC = re.compile(r"cycle (\w+): sources unchanged=(\d+) resumed=(\d+) fresh=(\d+) reparsed=(\d+) failed=(\d+) stale=(\d+); "
                 r"wrote logs=(\d+) parts=(\d+) built=(\d+) hot=(\d+) cold=(\d+) in (\S+) err=(.*)$")

def dur(s):
    tot = 0.0
    for v, u in re.findall(r"([\d.]+)(ms|µs|us|h|m|s)", s):
        tot += float(v) * {"ms": 1e-3, "µs": 1e-6, "us": 1e-6, "h": 3600, "m": 60, "s": 1}[u]
    return tot

def cpu(pid):
    f = open("/proc/%d/stat" % pid).read().rsplit(")", 1)[1].split()
    return (int(f[11]) + int(f[12])) / TCK

serve = subprocess.Popen(["systemd-run", "--scope", "--quiet", "-p", "MemoryMax=8G", "env", "GOMAXPROCS=4",
                          BIN, "serve", "--config", CFG + "/server.toml"],
                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
lines = queue.Queue()
log = open(P + "-serve.log", "w")
def reader():
    for l in serve.stdout:
        log.write(l); log.flush()
        m = CYC.search(l)
        if m:
            lines.put(m.groups())
threading.Thread(target=reader, daemon=True).start()
time.sleep(1)
pid = serve.pid
assert open("/proc/%d/comm" % pid).read().strip() == "mnemonic", "serve pid is not the mnemonic process"
ps = subprocess.Popen(["pidstat", "-u", "-r", "-h", "-p", str(pid), "1"], stdout=open(P + "-pidstat.txt", "w"))

def next_cycle(limit=1800):
    return lines.get(timeout=limit)

t0 = time.time()
b = next_cycle(7200)
boot_cpu = cpu(pid)
print("boot: reason=%s unchanged=%s resumed=%s fresh=%s reparsed=%s built=%s in %.1f s cpu %.1f s err=%s"
      % (b[0], b[1], b[2], b[3], b[4], b[9], dur(b[12]), boot_cpu, b[13]))
while True:
    try:
        b = lines.get(timeout=SPACING)
        print("extra cycle after boot: reason=%s in %.1f s" % (b[0], dur(b[12])))
    except queue.Empty:
        break

prof = None
if PERF:
    prof = subprocess.Popen(["perf", "record", "-F", "199", "-g", "-p", str(pid), "-o", P + "-perf.data"],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
rows, errs = [], 0
for i in range(1, N + 1):
    c0 = cpu(pid); time.sleep(SPACING); idle = cpu(pid) - c0
    m = json.load(open(man_path))
    body = "".join(json.dumps({"type": "user", "sessionId": sid, "uuid": str(uuid.uuid4()),
                               "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime()),
                               "cwd": "/tmp/p2probe", "message": {"role": "user", "content": "qz9p2probe %d %d" % (i, k)}},
                              separators=(",", ":")) + "\n" for k in range(10)).encode()
    q = urllib.parse.urlencode({"offset": m["size"], "head_hash": m["head_hash"], "source_name": m.get("source_name") or "claude",
                                "path": m.get("path") or "/tmp/p2probe", "raw_end": 0})
    req = urllib.request.Request("http://127.0.0.1:%s/v1/raw/%s/%s/%s?%s" % (PORT, machine, harness, src, q),
                                 data=body, method="PUT", headers={"Authorization": "Bearer " + token,
                                                                    "Content-Type": "application/octet-stream"})
    c1 = cpu(pid); w1 = time.time()
    try:
        code = urllib.request.urlopen(req, timeout=60).status
    except urllib.error.HTTPError as e:
        code = e.code
    if code != 200:
        errs += 1
        print("round %d: PUT %d" % (i, code)); continue
    g = next_cycle()
    c2 = cpu(pid); w2 = time.time()
    changed = int(g[2]) + int(g[3]) + int(g[4])
    ok = changed >= 1 and g[13].strip() == "<nil>"
    errs += 0 if ok else 1
    rows.append((i, c2 - c1, dur(g[12]), w2 - w1, idle, g[0], changed, int(g[9]), int(g[8]), ok))
if prof:
    prof.send_signal(2); prof.wait()
ps.terminate()
serve.terminate(); serve.wait(timeout=120)

print("round\tcycle_cpu_s\tcycle_wall_s\tput_to_cycle_end_s\tidle_cpu_s_per_%ds\treason\tchanged\tbuilt\tpart_bytes\tok" % SPACING)
for r in rows:
    print("%d\t%.2f\t%.3f\t%.3f\t%.2f\t%s\t%d\t%d\t%d\t%d" % (r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7], r[8], r[9]))
cc = [r[1] for r in rows]; cw = [r[2] for r in rows]
if cc:
    print("cycle_cpu_s median %.2f range %.2f-%.2f n=%d | cycle_wall_s median %.3f range %.3f-%.3f | errors %d of %d"
          % (statistics.median(cc), min(cc), max(cc), len(cc), statistics.median(cw), min(cw), max(cw), errs, N))
else:
    print("no rounds measured, errors %d of %d" % (errs, N))
print("evidence %s-{serve.log,pidstat.txt}" % P)
if prof:
    rep = subprocess.run(["perf", "report", "-i", P + "-perf.data", "--children", "--stdio", "--no-demangle", "-g", "none",
                          "--percent-limit", "0.5"], capture_output=True, text=True).stdout
    for l in rep.splitlines():
        if re.search(r"archiveSetFor|buildPart|ingestArchive|restreamMembers|rematerialize|\.cycle$|Ingest$|windowSHA", l):
            print("perf " + " ".join(l.split()))
PY
