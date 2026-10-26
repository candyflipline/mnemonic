#!/bin/sh
# perf-p2.sh BIN DIR — CPU of a mixed-component append cycle in a running serve.
#
# The engine run's case M (one raw source sharing a session id with archive ch
# members, 10 lines appended, one cycle) started a fresh process per round, so
# each round paid archiveSetFor's first-call scan of every ch file. serve is one
# long-lived process. This measures that shape: one `BIN serve` on DIR, its boot
# cycle, then N rounds of one 10-line PUT append to each of TARGETS mixed
# sources and the "cycle put" that indexes them. With TARGETS=9 a round is the
# cycle PROF saw stall in rematerialize and OOM on 83b0042.
#
# Prep, once per DIR (idempotent, outside the timed part):
#   - DIR: an overlayfs view of SRC (default $BOX/http-1, a raw-only store, never
#     written: writes land in DIR.up); the prep stops if / drops under FLOOR GB free
#   - DIR/events: a symlink tree to the ch files under $BOX/events (read-only)
#     smaller than CH_MAX bytes (default all 16)
#   - one `BIN index ingest -settle` brings the archive in and settles every
#     source, so archive member logs are retired as on prod
#   - the targets: the first TARGETS raw (kind 0) claude-code checkpoints of one
#     author whose sessions meet an archive member's; a token for that author
# Run: serve at GOMAXPROCS=4 under MemoryMax=MEM (8G, prod's shape) in its own
# scope, pidstat 1 s alongside. Per round: SPACING s idle (its CPU is printed),
# per target read the manifest and PUT 10 synthetic claude-code lines at
# offset=size, wait for the next cycle line; cycle CPU = serve utime+stime from
# just before the first PUT to that line. Work check per round: every PUT 200,
# the cycle counts resumed+fresh+reparsed >= 1, err=<nil>. A serve that dies
# (OOM) ends the run and is reported with the scope's memory.peak and the
# kernel's oom_kill count. Prints one TSV row per round, n, median and range,
# and the scope's memory.peak.
# PERF=1 wraps the rounds in `perf record -g` (an unreported profile run) and
# prints the share of samples under archiveSetFor and buildPart.
# Counts, sizes and timings only. Run under the box lock:
#   flock /var/lib/mnu/box.lock sh perf-p2.sh BIN DIR
# RESET=1 runs on a fresh overlay over the prepped DIR.up (DIR unmounted), so
# each run starts from the same settled store. SETTLE=1 then stops serve and
# runs one `index ingest -settle` on that run's store under MemoryMax=SETTLE_MEM:
# after two rounds the targets' hot parts are deltas, and this settle cycle is
# the one whose rematerialize opened the holder once per member (wall, max RSS,
# exit status or the killing signal printed).
# Env: BOX (/var/lib/mnu), SRC ($BOX/http-1), OUT ($BOX/out), N (6),
#      SPACING (20), PORT (7795), PERF (0), CH_MAX (all), TARGETS (9),
#      MEM (8G), FLOOR (15), RESET (0), SETTLE (0), SETTLE_MEM (16G),
#      PREP_ONLY (0: 1 stops after the prep).
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
RESET=${RESET:-0}
SETTLE=${SETTLE:-0}
SETTLE_MEM=${SETTLE_MEM:-16G}
CH_MAX=${CH_MAX:-1000000000000}
TARGETS=${TARGETS:-9}
MEM=${MEM:-8G}
FLOOR=${FLOOR:-15}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
SHA8=$(sha256sum "$BIN" | cut -c1-8)
P=$OUT/p2-$SHA8-$STAMP
mkdir -p "$OUT"

avail=$(awk '/MemAvailable/ {print int($2/1048576)}' /proc/meminfo)
[ "$avail" -ge 8 ] || { echo "MemAvailable ${avail} GB < 8 GB, refusing" >&2; exit 2; }

freegb() { df --output=avail -B1G / | tail -1 | tr -d ' '; }
RUN=$DIR
if [ "$RESET" = 1 ]; then
	[ -e "$DIR.up/.p2-settled" ] || { echo "RESET=1 needs a prepped $DIR (run once without RESET)" >&2; exit 2; }
	! mountpoint -q "$DIR" || umount "$DIR"
	RUN=$DIR-r$STAMP
	mkdir -p "$RUN" "$RUN.up" "$RUN.work"
	mount -t overlay overlay -o "lowerdir=$DIR.up:$SRC,upperdir=$RUN.up,workdir=$RUN.work" "$RUN"
	mkdir -p "$P-cfg"
	chmod 700 "$P-cfg"
	sed "s#^data_dir = .*#data_dir = \"$RUN\"#" "$RUN/.p2-cfg/server.toml" > "$P-cfg/server.toml"
	cp "$RUN/.p2-cfg/target" "$RUN/.p2-cfg/token.out" "$P-cfg/"
	CFG=$P-cfg
	echo "reset: fresh overlay $RUN over the prepped $DIR.up"
elif ! mountpoint -q "$DIR"; then
	mkdir -p "$DIR" "$DIR.up" "$DIR.work"
	mount -t overlay overlay -o "lowerdir=$SRC,upperdir=$DIR.up,workdir=$DIR.work" "$DIR"
fi
if [ "$RESET" != 1 ] && [ ! -e "$DIR/.p2-settled" ]; then
	mkdir -p "$DIR/events"
	cp "$BOX/events/.parser-version" "$DIR/events/" 2>/dev/null || true
	find "$BOX/events" -name 'ch-*.parquet' -size -"$CH_MAX"c | while IFS= read -r f; do
		rel=${f#"$BOX/events/"}
		mkdir -p "$DIR/events/$(dirname "$rel")"
		[ -e "$DIR/events/$rel" ] || ln -s "$f" "$DIR/events/$rel"
	done
fi
CHB=$(find -L "$RUN/events" -name 'ch-*.parquet' -printf '%s\n' | awk '{s+=$1} END {print s+0}')
echo "ch files: $(find "$RUN/events" -name 'ch-*.parquet' | wc -l) of $(find "$BOX/events" -name 'ch-*.parquet' | wc -l), bytes $CHB of $(find "$BOX/events" -name 'ch-*.parquet' -printf '%s\n' | awk '{s+=$1} END {print s+0}')"
if [ ! -e "$RUN/.p2-settled" ]; then
	need=$((FLOOR + 2 * CHB / 1000000000 + 4))
	[ "$(freegb)" -ge "$need" ] || { echo "df: $(freegb) GB free < $need (FLOOR + twice the ch bytes + 4)" >&2; exit 2; }
	PU=p2-prep-$STAMP
	systemd-run --unit="$PU" --quiet -p MemoryMax=12G -p WorkingDirectory=/ --setenv=GOMAXPROCS=4 \
		/usr/bin/time.gnu -v -o "$P-prep.time" "$BIN" index ingest -root "$DIR" -store "$DIR/index" -settle
	while systemctl is-active -q "$PU"; do
		echo "$(date -u +%T) free_gb $(freegb) upper_gb $(du -s -B1G "$DIR.up" | cut -f1)" >> "$P-prep-df.log"
		if [ "$(freegb)" -lt "$FLOOR" ]; then
			systemctl stop "$PU"
			echo "DISK GUARD: $(freegb) GB free < $FLOOR, prep stopped" >&2
			exit 2
		fi
		sleep 10
	done
	journalctl -u "$PU" -o cat --no-pager | grep -E '^ingest:' || true
	grep -E 'Exit status|Elapsed .wall|Maximum resident|Percent of CPU' "$P-prep.time"
	grep -q 'Exit status: 0' "$P-prep.time" || { echo "prep failed" >&2; exit 1; }
	touch "$DIR/.p2-settled"
fi

[ "$RESET" = 1 ] || CFG=$DIR/.p2-cfg
if [ ! -e "$CFG/server.toml" ]; then
	mkdir -p "$CFG"
	chmod 700 "$CFG"
	python3 -I - "$DIR/index/src" "$CFG" "$TARGETS" <<'PY'
import json, os, sys
src, cfg, k = sys.argv[1], sys.argv[2], int(sys.argv[3])
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
author = cands[0]["key"].split("__", 1)[0]
picked = [c for c in cands if c["key"].split("__", 1)[0] == author][:k]
with open(cfg + "/target", "w") as out:
    for c in picked:
        sid = sorted(arch.intersection(c["sessions"]))[0]
        out.write("%s %s %s %s %s\n" % (tuple(c["key"].split("__", 3)) + (sid,)))
print("mixed raw candidates=%d archive sessions=%d picked=%d rows=%d" % (
    len(cands), len(arch), len(picked), sum(c.get("rows", 0) for c in picked)))
PY
	read -r AUTH _ < "$CFG/target"
	"$BIN" token new --author "$AUTH" > "$CFG/token.out"
	{
		echo "data_dir = \"$DIR\""
		echo "listen = \"127.0.0.1:$PORT\""
		tail -n +2 "$CFG/token.out"
	} > "$CFG/server.toml"
fi

[ "${PREP_ONLY:-0}" != 1 ] || exit 0
python3 -u -I - "$BIN" "$RUN" "$CFG" "$P" "$N" "$SPACING" "$PORT" "$PERF" "$MEM" "p2-serve-$STAMP" <<'PY'
import json, os, queue, re, statistics, subprocess, sys, threading, time, urllib.request, uuid
BIN, DIR, CFG, P, N, SPACING, PORT, PERF, MEM, UNIT = sys.argv[1:11]
N, SPACING, PERF = int(N), int(SPACING), PERF == "1"
targets = [l.split() for l in open(CFG + "/target") if l.strip()]
token = open(CFG + "/token.out").readline().strip()
CG = "/sys/fs/cgroup/system.slice/%s.scope/" % UNIT

seen = {"peak": float("nan"), "oom": -1}

def sample():
    while True:
        try:
            seen["peak"] = int(open(CG + "memory.peak").read()) / 2**30
            seen["oom"] = int(dict(l.split() for l in open(CG + "memory.events"))["oom_kill"])
        except (OSError, KeyError, ValueError):
            pass
        time.sleep(0.5)

def peak_gb():
    return seen["peak"]

def ooms():
    return seen["oom"]
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

serve = subprocess.Popen(["systemd-run", "--scope", "--quiet", "--unit=" + UNIT, "-p", "MemoryMax=" + MEM, "env", "GOMAXPROCS=4",
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
threading.Thread(target=sample, daemon=True).start()
time.sleep(1)
pid = serve.pid
assert open("/proc/%d/comm" % pid).read().strip() == "mnemonic", "serve pid is not the mnemonic process"
ps = subprocess.Popen(["pidstat", "-u", "-r", "-h", "-p", str(pid), "1"], stdout=open(P + "-pidstat.txt", "w"))

def next_cycle(limit=3600):
    end = time.time() + limit
    while time.time() < end:
        try:
            return lines.get(timeout=5)
        except queue.Empty:
            if serve.poll() is not None:
                break
    print("serve died or no cycle: rc=%s memory.peak_gb=%.2f oom_kill=%d after %.0f s" % (serve.poll(), peak_gb(), ooms(), time.time() - t0))
    print("evidence %s-{serve.log,pidstat.txt}" % P)
    sys.exit(3)

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
    c1 = cpu(pid); w1 = time.time()
    bad = 0
    for author, machine, harness, src, sid in targets:
        m = json.load(open("%s/raw/author=%s/machine=%s/harness=%s/%s.json" % (DIR, author, machine, harness, src)))
        body = "".join(json.dumps({"type": "user", "sessionId": sid, "uuid": str(uuid.uuid4()),
                                   "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime()),
                                   "cwd": "/tmp/p2probe", "message": {"role": "user", "content": "qz9p2probe %d %d" % (i, k)}},
                                  separators=(",", ":")) + "\n" for k in range(10)).encode()
        q = urllib.parse.urlencode({"offset": m["size"], "head_hash": m["head_hash"], "source_name": m.get("source_name") or "claude",
                                    "path": m.get("path") or "/tmp/p2probe", "raw_end": 0})
        req = urllib.request.Request("http://127.0.0.1:%s/v1/raw/%s/%s/%s?%s" % (PORT, machine, harness, src, q),
                                     data=body, method="PUT", headers={"Authorization": "Bearer " + token,
                                                                        "Content-Type": "application/octet-stream"})
        try:
            code = urllib.request.urlopen(req, timeout=60).status
        except urllib.error.HTTPError as e:
            code = e.code
        bad += code != 200
    if bad:
        errs += 1
        print("round %d: %d PUTs not 200" % (i, bad)); continue
    g = next_cycle()
    c2 = cpu(pid); w2 = time.time()
    changed = int(g[2]) + int(g[3]) + int(g[4])
    ok = changed >= 1 and g[13].strip() == "<nil>"
    errs += 0 if ok else 1
    rows.append((i, c2 - c1, dur(g[12]), w2 - w1, idle, g[0], changed, int(g[9]), int(g[8]), ok))
if prof:
    prof.send_signal(2); prof.wait()
ps.terminate()
final_peak, final_oom = peak_gb(), ooms()
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
print("serve memory.peak_gb %.2f oom_kill %d targets %d" % (final_peak, final_oom, len(targets)))
print("evidence %s-{serve.log,pidstat.txt}" % P)
if prof:
    rep = subprocess.run(["perf", "report", "-i", P + "-perf.data", "--children", "--stdio", "--no-demangle", "-g", "none",
                          "--percent-limit", "0.5"], capture_output=True, text=True).stdout
    for l in rep.splitlines():
        if re.search(r"archiveSetFor|buildPart|ingestArchive|restreamMembers|rematerialize|\.cycle$|Ingest$|windowSHA", l):
            print("perf " + " ".join(l.split()))
PY

if [ "$SETTLE" = 1 ]; then
	SU=p2-settle-$STAMP
	systemd-run --unit="$SU" --quiet -p MemoryMax="$SETTLE_MEM" -p WorkingDirectory=/ --setenv=GOMAXPROCS=4 \
		/usr/bin/time.gnu -v -o "$P-settle.time" "$BIN" index ingest -root "$RUN" -store "$RUN/index" -settle
	while systemctl is-active -q "$SU"; do
		sleep 5
	done
	echo "settle cycle (index ingest -settle, GOMAXPROCS=4, MemoryMax=$SETTLE_MEM):"
	journalctl -u "$SU" -o cat --no-pager | grep -E '^ingest:' || true
	grep -E 'Command terminated|Exit status|Elapsed .wall|Maximum resident|Percent of CPU' "$P-settle.time"
fi
[ "$RESET" != 1 ] || umount "$RUN"
