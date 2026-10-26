#!/bin/sh
# lag.sh BIN DIR — append lag of a running serve + `push --watch`, N probes.
#
# DIR is a serve data_dir that already holds a full store (cp -a of a
# push-run.sh store: the index lag depends on the source count, ~8.8k here).
# THE PROBES WRITE TO IT: lag.sh adds one source (a probe file under a fresh
# HOME) to DIR/raw, so run storesha/equiv before it or point it at a cp -a copy.
#
# Starts `BIN serve` on DIR (GOMAXPROCS=4) and waits for a quiet cycle (changed
# = resumed+fresh+reparsed = 0, stale = 0), starts `BIN push --watch` on a fresh
# HOME holding one probe file under .claude/projects/, waits for its first push
# and the cycle that counts it, then N times (spaced >= SPACING s from the
# previous write): append one JSONL line to the probe file with one write(2),
# and record, from that write
#   ack_ms    until DIR/raw/.../<src_id>.json shows the new Size (manifest polled
#             every 5 ms; the manifest is the server's durable state, renamed
#             into place after the frame fsync),
#   index_ms  until the first serve log line "cycle ...: sources ... resumed=a
#             fresh=b reparsed=c" with a+b+c >= 1 that arrives after the write.
#             Serve stdout is read through a pipe and each line stamped on
#             arrival, because the log's own stamps are whole seconds. A cycle
#             logs at its END, so this is write -> cycle end.
# Prints one TSV row per probe, then median and range of each, the quiet-wait,
# and the cycle durations seen. pidstat -u -d (serve, push) and vmstat run 1 s
# alongside to $OUT/base-<name>-lag-*.txt. Hold the box lock for the whole run:
#   flock /var/lib/mnu/box.lock sh lag.sh BIN DIR
# Env: BOX (/var/lib/mnu), OUT ($BOX/out), N (10), SPACING (15), PORT (7793),
#      MACHINE (plainbox), AUTHOR (uplbox), QUIET_MAX (3600 s wait for the quiet cycle).
set -eu

BIN=${1:?usage: lag.sh BIN DIR}
DIR=${2:?usage: lag.sh BIN DIR}
BOX=${BOX:-/var/lib/mnu}
OUT=${OUT:-$BOX/out}
N=${N:-10}
SPACING=${SPACING:-15}
PORT=${PORT:-7793}
MACHINE=${MACHINE:-plainbox}
AUTHOR=${AUTHOR:-uplbox}
QUIET_MAX=${QUIET_MAX:-3600}
NAME=$(basename "$DIR")
P=$OUT/base-$NAME-lag
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
[ -d "$DIR/raw" ] || { echo "$DIR/raw missing: lag.sh needs a populated store" >&2; exit 2; }
HOME_DIR=$BOX/laghome-$NAME
CFG=$P-cfg
[ -e "$HOME_DIR" ] && { echo "$HOME_DIR exists" >&2; exit 1; }
[ -e "$CFG" ] && { echo "$CFG exists" >&2; exit 1; }
mkdir -p "$HOME_DIR/.claude/projects/lagprobe" "$CFG"
chmod 700 "$CFG"

"$BIN" token new --author "$AUTHOR" > "$CFG/token.out"
head -1 "$CFG/token.out" > "$CFG/token"
chmod 600 "$CFG/token"
{
	echo "data_dir = \"$DIR\""
	echo "listen = \"127.0.0.1:$PORT\""
	tail -n +2 "$CFG/token.out"
} > "$CFG/server.toml"
cat > "$CFG/client.toml" <<CFGEOF
server = "http://127.0.0.1:$PORT"
token_file = "$CFG/token"
machine = "$MACHINE"
CFGEOF

python3 -I - "$BIN" "$DIR" "$CFG" "$HOME_DIR" "$P" "$N" "$SPACING" "$PORT" "$MACHINE" "$AUTHOR" "$QUIET_MAX" <<'PYEOF'
import hashlib, json, os, re, signal, statistics, subprocess, sys, threading, time
bin_, data, cfg, home, pfx, n, spacing, port, machine, author, quiet_max = sys.argv[1:12]
n, spacing, quiet_max = int(n), float(spacing), float(quiet_max)
probe = os.path.join(home, ".claude/projects/lagprobe/lag.jsonl")
src = hashlib.sha256(probe.encode()).hexdigest()[:16]
manifest = os.path.join(data, "raw", "author=" + author, "machine=" + machine, "harness=claude-code", src + ".json")

lines = []
lock = threading.Lock()
serve = subprocess.Popen([bin_, "serve", "--config", cfg + "/server.toml"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                         env=dict(os.environ, GOMAXPROCS="4"), text=True, bufsize=1)
def reader():
    for l in serve.stdout:
        with lock:
            lines.append((time.time(), l.rstrip("\n")))
threading.Thread(target=reader, daemon=True).start()

CYC = re.compile(r"cycle (\w+): sources unchanged=(\d+) resumed=(\d+) fresh=(\d+) reparsed=(\d+) failed=(\d+) stale=(\d+);.* in ([0-9.]+)(ms|s|m[0-9.]*s?) err=(.*)$")
def dur(v, u):
    if u == "ms": return float(v) / 1000
    if u == "s": return float(v)
    return float(v) * 60
def cycles():
    with lock:
        snap = list(lines)
    out = []
    for t, l in snap:
        m = CYC.search(l)
        if m:
            out.append((t, l, m.group(1), int(m.group(3)) + int(m.group(4)) + int(m.group(5)), int(m.group(7)), dur(m.group(8), m.group(9)), m.group(10)))
    return out

def wait(pred, limit, what):
    end = time.time() + limit
    while time.time() < end:
        r = pred()
        if r:
            return r
        if serve.poll() is not None:
            sys.exit("serve exited while waiting for " + what)
        time.sleep(0.05)
    sys.exit("timeout waiting for " + what)

t_start = time.time()
wait(lambda: any("listening on" in l for _, l in list(lines)), 60, "listening")
q0 = time.time()
def quiet():
    cs = cycles()
    return next((c for c in cs if c[3] == 0 and c[4] == 0), None)
qc = wait(quiet, quiet_max, "a quiet cycle")
quiet_wait = qc[0] - t_start
print("boot_to_first_quiet_cycle_s\t%.1f" % quiet_wait)
print("cycles_before_quiet\t%d" % len([c for c in cycles() if c[0] <= qc[0]]))

def line(i):
    return (json.dumps({"type": "user", "uuid": "00000000-0000-4000-8000-%012d" % i, "sessionId": "00000000-0000-4000-8000-0000000000aa",
                        "timestamp": "2026-10-25T12:00:%02dZ" % (i % 60), "cwd": "/probe", "version": "2.0.0", "parentUuid": None,
                        "message": {"role": "user", "content": "lag probe %d" % i}}) + "\n").encode()

def manifest_size():
    try:
        with open(manifest) as f:
            return json.load(f)["size"]
    except (OSError, ValueError, KeyError):
        return -1

fd = os.open(probe, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
first = line(0)
os.write(fd, first)
total = len(first)
pidlog = open(pfx + "-pidstat.txt", "w")
vm = subprocess.Popen(["vmstat", "1"], stdout=open(pfx + "-vmstat.txt", "w"))
push = subprocess.Popen([bin_, "push", "--watch", "--config", cfg + "/client.toml"], env=dict(os.environ, HOME=home),
                        stdout=open(pfx + "-client.out", "w"), stderr=subprocess.STDOUT)
ps = subprocess.Popen(["pidstat", "-u", "-d", "-h", "-p", "%d,%d" % (serve.pid, push.pid), "1"], stdout=pidlog)
try:
    t_first = time.time()
    wait(lambda: manifest_size() >= total, 60, "first push of the probe")
    wait(lambda: next((c for c in cycles() if c[0] > t_first and c[3] >= 1), None), 120, "the cycle that counts the first push")
    rows = []
    prev = time.time()
    for i in range(1, n + 1):
        gap = prev + spacing - time.time()
        if gap > 0:
            time.sleep(gap)
        b = line(i)
        t0 = time.time()
        os.write(fd, b)
        total += len(b)
        prev = t0
        ack = None
        end = t0 + 30
        while time.time() < end:
            if manifest_size() >= total:
                ack = time.time()
                break
            time.sleep(0.005)
        cyc = None
        end = t0 + 120
        while time.time() < end:
            cyc = next((c for c in cycles() if c[0] > t0 and c[3] >= 1), None)
            if cyc:
                break
            time.sleep(0.02)
        rows.append((i, None if ack is None else (ack - t0) * 1000, None if cyc is None else (cyc[0] - t0) * 1000, cyc))
        print("probe\t%d\tack_ms\t%s\tindex_ms\t%s\tcycle_s\t%s\tchanged\t%s" % (
            i, "%.0f" % rows[-1][1] if rows[-1][1] is not None else "MISSING", "%.0f" % rows[-1][2] if rows[-1][2] is not None else "MISSING",
            "%.2f" % cyc[5] if cyc else "", cyc[3] if cyc else ""))
        sys.stdout.flush()
    for name, k in (("ack_ms", 1), ("index_ms", 2)):
        xs = sorted(r[k] for r in rows if r[k] is not None)
        if xs:
            print("%s\tmedian %.0f\tmin %.0f\tmax %.0f\tn %d of %d" % (name, statistics.median(xs), xs[0], xs[-1], len(xs), n))
    cs = [c for c in cycles() if c[0] > qc[0]]
    ds = sorted(c[5] for c in cs)
    if ds:
        print("cycle_s_after_quiet\tmedian %.2f\tmin %.2f\tmax %.2f\tn %d" % (statistics.median(ds), ds[0], ds[-1], len(ds)))
    print("cycle_err_not_nil\t%d" % len([c for c in cycles() if c[6] != "<nil>"]))
    with open(pfx + "-cycles.txt", "w") as f:
        for c in cycles():
            f.write("%.3f\t%s\n" % (c[0], c[1][:300]))
finally:
    for p in (push, serve):
        if p.poll() is None:
            p.send_signal(signal.SIGTERM)
    for p in (push, serve):
        try:
            p.wait(timeout=120)
        except subprocess.TimeoutExpired:
            print("process %d did not stop in 120 s" % p.pid, file=sys.stderr)
    ps.terminate(); vm.terminate()
PYEOF
