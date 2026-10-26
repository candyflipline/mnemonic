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
# and the cycle that counts it, appends warm-up lines until a counting cycle takes
# under 20 s (the first put cycles after boot convert every source quiet for
# settleSource, 15 min, from its hot part to a cold one, minutes of work that is
# not the steady state), then N times (spaced >= SPACING s from the
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
#      MACHINE (plainbox), AUTHOR (uplbox), QUIET_MAX (3600 s wait for the quiet cycle),
#      TAG (suffix for the output and HOME names of a repeat run on the same DIR).
# GEQ additions (lane/upl-geq):
#   SWEEP=1         do not wait for a quiet cycle: probe while the boot sweep
#                   (version-stale sources) runs. Each probe row says whether the
#                   first quiet cycle had been logged yet (sweeping=1/0); index_ms
#                   is then not meaningful (every cycle reparses) and is printed
#                   only as logged.
#   STRACE_OPENAT=1 serve runs under strace -f --seccomp-bpf -ttt -y -e trace=openat,open
#                   (to $P-openat.txt); each probe's window [write, end of the
#                   first cycle that counts it) is written to $P-probes.tsv and
#                   the openat calls inside it on raw/**/*.json and *.jsonl.zst
#                   are counted per probe ("opens" rows). Cycles seen in the window
#                   are listed too: a second cycle racing the probe is counted.
#   FIRST_MAX, IDX_MAX  seconds to wait for the cycle that counts the first push
#                   (default 1800) and for each probe's cycle (default 120); raise
#                   them on a store whose cycles run long (a sweep).
#   SERVE_WRAP      words put in front of the serve command, for example
#                   "systemd-run --scope -q -p MemoryMax=18G -p MemorySwapMax=0" so a
#                   serve that blows up is killed alone instead of by the OOM killer.
#   PUSH_PORT       the final tree's gRPC listener (server.toml push_listen, required
#                   there); the client's server URL then points at it.
#   ack_ms and index_ms print median, min, max, p95, p99 (nearest rank).
#   ROWS            JSONL lines each probe appends in its one write(2) (default 1).
# P4B additions (prod-shaped hot load):
#   HOME_KEEP       a fixed client HOME (from hot-prep.sh), reused across runs so the
#                   hot sources keep their ids and the client resumes them.
#   HOT_EVERY       seconds between keep-alive appends to the HOT_ACTIVE files
#                   (indices of hot-NN, default "5 6"); every HOT_IDLE s (30) one
#                   file of HOT_IDLE_SET (indices; default every other hot-NN)
#                   gets one line, round robin, so each stays
#                   hot (settleSource is 15 min). Each appended line is the file's
#                   last user/assistant line with a fresh uuid. index_ms then counts
#                   to the end of the first cycle that started after the ack.
#                   Steady rows (cycles ending after the first probe) print wall
#                   and hot= median and range.
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
P=$OUT/base-$NAME-lag${TAG:+-$TAG}
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
[ -d "$DIR/raw" ] || { echo "$DIR/raw missing: lag.sh needs a populated store" >&2; exit 2; }
HOME_DIR=${HOME_KEEP:-$BOX/laghome-$NAME${TAG:+-$TAG}}
CFG=$P-cfg
[ -z "${HOME_KEEP:-}" ] && [ -e "$HOME_DIR" ] && { echo "$HOME_DIR exists" >&2; exit 1; }
[ -e "$CFG" ] && { echo "$CFG exists" >&2; exit 1; }
mkdir -p "$HOME_DIR/.claude/projects/lagprobe" "$CFG"
chmod 700 "$CFG"

"$BIN" token new --author "$AUTHOR" > "$CFG/token.out"
head -1 "$CFG/token.out" > "$CFG/token"
chmod 600 "$CFG/token"
{
	echo "data_dir = \"$DIR\""
	echo "listen = \"127.0.0.1:$PORT\""
	[ -z "${PUSH_PORT:-}" ] || echo "push_listen = \"127.0.0.1:$PUSH_PORT\""
	tail -n +2 "$CFG/token.out"
} > "$CFG/server.toml"
cat > "$CFG/client.toml" <<CFGEOF
server = "http://127.0.0.1:${PUSH_PORT:-$PORT}"
token_file = "$CFG/token"
machine = "$MACHINE"
CFGEOF

python3 -u -I - "$BIN" "$DIR" "$CFG" "$HOME_DIR" "$P" "$N" "$SPACING" "$PORT" "$MACHINE" "$AUTHOR" "$QUIET_MAX" "${SWEEP:-0}" "${STRACE_OPENAT:-0}" "${ROWS:-1}" <<'PYEOF'
import hashlib, json, os, re, signal, statistics, subprocess, sys, threading, time
bin_, data, cfg, home, pfx, n, spacing, port, machine, author, quiet_max, sweep, stropen, nrows = sys.argv[1:15]
nrows = int(nrows)
sweep, stropen = sweep == "1", stropen == "1"
n, spacing, quiet_max = int(n), float(spacing), float(quiet_max)
probe = os.path.join(home, ".claude/projects/lagprobe/lag-%d.jsonl" % int(time.time()))
src = hashlib.sha256(probe.encode()).hexdigest()[:16]
manifest = os.path.join(data, "raw", "author=" + author, "machine=" + machine, "harness=claude-code", src + ".json")

lines = []
lock = threading.Lock()
cmd = [bin_, "serve", "--config", cfg + "/server.toml"]
if stropen:
    cmd = ["strace", "-f", "--seccomp-bpf", "-ttt", "-y", "-e", "signal=none", "-e", "trace=openat,open", "-o", pfx + "-openat.txt"] + cmd
cmd = os.environ.get("SERVE_WRAP", "").split() + cmd
serve = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                         env=dict(os.environ, GOMAXPROCS="4"), text=True, bufsize=1)
servelog = open(pfx + "-serve.log", "w")
def reader():
    for l in serve.stdout:
        with lock:
            lines.append((time.time(), l.rstrip("\n")))
        servelog.write("%.3f %s" % (time.time(), l))
        servelog.flush()
threading.Thread(target=reader, daemon=True).start()

CYC = re.compile(r"cycle (\w+): sources unchanged=(\d+) resumed=(\d+) fresh=(\d+) reparsed=(\d+) failed=(\d+) stale=(\d+);.* hot=(\d+) cold=\d+ in ([0-9.]+)(ms|s|m[0-9.]*s?) err=(.*)$")
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
            out.append((t, l, m.group(1), int(m.group(3)) + int(m.group(4)) + int(m.group(5)), int(m.group(7)), dur(m.group(9), m.group(10)), m.group(11), int(m.group(8))))
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
wait(lambda: any(("listening on" in l or "mnemonic serve: HTTP" in l) for _, l in list(lines)), float(os.environ.get("LISTEN_MAX", "60")), "listening")
q0 = time.time()
def quiet():
    cs = cycles()
    return next((c for c in cs if c[3] == 0 and c[4] == 0), None)
real = serve
if stropen:
    def child():
        r = subprocess.run(["pgrep", "-P", str(serve.pid)], capture_output=True, text=True)
        return int(r.stdout.split()[0]) if r.stdout.split() else None
    real_pid = wait(child, 30, "the serve child of strace")
else:
    real_pid = serve.pid
if sweep:
    qc = (t_start, "", "", 0, 0, 0.0, "")
    print("sweep_mode\t1: probing from boot, quiet cycle not awaited")
else:
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
hot_every = float(os.environ.get("HOT_EVERY", "0"))
hot_stop = threading.Event()
hot_count = [0, 0]
if hot_every > 0:
    import glob, uuid
    hot_files = sorted(glob.glob(os.path.join(home, ".claude/projects/hot-*/*.jsonl")), key=lambda p: int(p.split("/")[-2][4:]))
    def template(path):
        with open(path, "rb") as f:
            f.seek(0, 2)
            f.seek(max(0, f.tell() - (8 << 20)))
            tail = f.read().split(b"\n")
        for raw in reversed(tail):
            try:
                o = json.loads(raw)
            except ValueError:
                continue
            if isinstance(o, dict) and o.get("type") in ("user", "assistant") and o.get("uuid") and o.get("sessionId"):
                return o
        return None
    hot = [(p, template(p)) for p in hot_files]
    hot = [(p, t) for p, t in hot if t is not None]
    active = [int(x) for x in os.environ.get("HOT_ACTIVE", "5 6").split()]
    act = [hot[i] for i in active if i < len(hot)]
    iset = os.environ.get("HOT_IDLE_SET")
    idle = [h for i, h in enumerate(hot) if i not in active and (iset is None or i in [int(x) for x in iset.split()])]
    print("hot_files\t%d\twith_template\t%d\tactive\t%d\tidle\t%d" % (len(hot_files), len(hot), len(act), len(idle)))
    def hot_append(h):
        p, t = h
        o = dict(t)
        o["parentUuid"], o["uuid"] = o["uuid"], str(uuid.uuid4())
        o["timestamp"] = time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime())
        t.clear(); t.update(o)
        try:
            hfd = os.open(p, os.O_WRONLY | os.O_APPEND)
            os.write(hfd, (json.dumps(o) + "\n").encode())
            os.close(hfd)
            hot_count[0] += 1
        except OSError:
            hot_count[1] += 1
    def keepalive():
        k = 0
        last_idle = 0.0
        for h in idle:
            hot_append(h)
        while not hot_stop.wait(hot_every):
            for h in act:
                hot_append(h)
            if idle and time.time() - last_idle >= float(os.environ.get("HOT_IDLE", "30")):
                hot_append(idle[k % len(idle)])
                k += 1
                last_idle = time.time()
    threading.Thread(target=keepalive, daemon=True).start()
ps = subprocess.Popen(["pidstat", "-u", "-d", "-h", "-p", "%d,%d" % (real_pid, push.pid), "1"], stdout=pidlog)
try:
    t_first = time.time()
    wait(lambda: manifest_size() >= total, 60, "first push of the probe")
    wait(lambda: next((c for c in cycles() if c[0] > t_first and c[3] >= 1), None), float(os.environ.get('FIRST_MAX', '1800')), "the cycle that counts the first push")
    warm = []
    for w in range(1, 7):
        b = line(100 + w)
        t0 = time.time()
        os.write(fd, b)
        total += len(b)
        wait(lambda: manifest_size() >= total, 60, "warm-up ack")
        cyc = wait(lambda: next((c for c in cycles() if c[0] > t0 and c[3] >= 1), None), 1800, "the warm-up cycle")
        warm.append(cyc[5])
        print("warmup\t%d\tcycle_s\t%.2f\tend_after_write_s\t%.1f" % (w, cyc[5], cyc[0] - t0))
        sys.stdout.flush()
        if cyc[5] < 20:
            break
        time.sleep(3)
    rows = []
    wins = []
    sweepflags = []
    prev = time.time()
    for i in range(1, n + 1):
        gap = prev + spacing - time.time()
        if gap > 0:
            time.sleep(gap)
        b = line(i) if nrows == 1 else b"".join(line(10000 + i * 100 + r) for r in range(nrows))
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
        end = t0 + float(os.environ.get('IDX_MAX', '120'))
        after = ack if (hot_every > 0 and ack is not None) else t0
        while time.time() < end:
            cyc = next((c for c in cycles() if c[0] > t0 and c[3] >= 1 and (hot_every == 0 or c[0] - c[5] >= after)), None)
            if cyc:
                break
            time.sleep(0.02)
        rows.append((i, None if ack is None else (ack - t0) * 1000, None if cyc is None else (cyc[0] - t0) * 1000, cyc))
        wins.append((i, t0, cyc[0] if cyc else None))
        qn = next((c for c in cycles() if c[3] == 0 and c[4] == 0), None)
        sweeping = 1 if (sweep and (qn is None or qn[0] > t0)) else 0
        print("probe\t%d\tack_ms\t%s\tindex_ms\t%s\tcycle_s\t%s\tchanged\t%s\tsweeping\t%d" % (
            i, "%.0f" % rows[-1][1] if rows[-1][1] is not None else "MISSING", "%.0f" % rows[-1][2] if rows[-1][2] is not None else "MISSING",
            "%.2f" % cyc[5] if cyc else "", cyc[3] if cyc else "", sweeping))
        sweepflags.append(sweeping)
        sys.stdout.flush()
    def pct(xs, p):
        return xs[max(0, min(len(xs) - 1, -(-int(p * len(xs)) // 100) - 1))]
    def summarize(label, sel):
        for name, k in (("ack_ms", 1), ("index_ms", 2)):
            xs = sorted(r[k] for r, f in zip(rows, sweepflags) if r[k] is not None and sel(f))
            if xs:
                print("%s%s\tmedian %.0f\tmin %.0f\tmax %.0f\tp95 %.0f\tp99 %.0f\tn %d of %d" % (name, label, statistics.median(xs), xs[0], xs[-1], pct(xs, 95), pct(xs, 99), len(xs), n))
    summarize("", lambda f: True)
    if sweep:
        summarize("_during_sweep", lambda f: f == 1)
        summarize("_after_sweep", lambda f: f == 0)
    if stropen:
        os_ = []
        for l in open(pfx + "-openat.txt", errors="replace"):
            m = re.match(r"^(?:\d+\s+)?([0-9.]+) open(?:at)?\(.*", l)
            if not m or "ENOENT" in l:
                continue
            q = re.findall(r"<([^>]*)>", l)
            p = re.findall(r'"([^"]*)"', l)
            path = p[-1] if p else ""
            if not q and not path.startswith("/"):
                continue
            full = path if path.startswith("/") else (q[0] if q else "") + "/" + path
            os_.append((float(m.group(1)), full))
        rawroot = os.path.join(data, "raw")
        with open(pfx + "-probes.tsv", "w") as f:
            f.write("probe\tt0\tcycle_end\n")
            for i, t0, te in wins:
                f.write("%d\t%.3f\t%s\n" % (i, t0, "%.3f" % te if te else ""))
        jm, zm, cyc_n = [], [], []
        for i, t0, te in wins:
            if te is None:
                continue
            j = z = 0
            for t, fp in os_:
                if t0 <= t <= te and fp.startswith(rawroot):
                    if fp.endswith(".json"):
                        j += 1
                    elif ".jsonl.zst" in fp:
                        z += 1
            nc = len([c for c in cycles() if t0 < c[0] <= te])
            jm.append(j); zm.append(z); cyc_n.append(nc)
            print("opens\t%d\tmanifests(.json)\t%d\tframes(.jsonl.zst)\t%d\tcycles_in_window\t%d" % (i, j, z, nc))
        if jm:
            print("opens_manifest\tmedian %.0f\tmin %d\tmax %d\tn %d" % (statistics.median(jm), min(jm), max(jm), len(jm)))
            print("opens_frame\tmedian %.0f\tmin %d\tmax %d\tn %d" % (statistics.median(zm), min(zm), max(zm), len(zm)))
    cs = [c for c in cycles() if c[0] > qc[0] and c[0] > t_first + 1]
    ds = sorted(c[5] for c in cs)
    if ds:
        print("cycle_s_after_quiet\tmedian %.2f\tmin %.2f\tmax %.2f\tn %d" % (statistics.median(ds), ds[0], ds[-1], len(ds)))
    st = [c for c in cycles() if wins and c[0] > wins[0][1]]
    if st:
        sw = sorted(c[5] for c in st)
        sh = sorted(c[7] for c in st)
        print("cycle_s_steady\tmedian %.2f\tmin %.2f\tmax %.2f\tn %d" % (statistics.median(sw), sw[0], sw[-1], len(sw)))
        print("hot_steady\tmedian %.0f\tmin %d\tmax %d\tn %d" % (statistics.median(sh), sh[0], sh[-1], len(sh)))
        print("kinds_steady\t%s" % " ".join("%s=%d" % (k, len([c for c in st if c[2] == k])) for k in sorted({c[2] for c in st})))
    if hot_every > 0:
        print("hot_appends\t%d\terrors\t%d" % tuple(hot_count))
    print("cycle_err_not_nil\t%d" % len([c for c in cycles() if c[6] != "<nil>"]))
finally:
    hot_stop.set()
    with open(pfx + "-cycles.txt", "w") as f:
        for c in cycles():
            f.write("%.3f\t%s\n" % (c[0], c[1][:300]))
    if push.poll() is None:
        push.send_signal(signal.SIGTERM)
    if serve.poll() is None:
        os.kill(real_pid, signal.SIGTERM)
    for p in (push, serve):
        try:
            p.wait(timeout=120)
        except subprocess.TimeoutExpired:
            print("process %d did not stop in 120 s" % p.pid, file=sys.stderr)
    ps.terminate(); vm.terminate()
PYEOF
