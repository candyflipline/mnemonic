#!/bin/sh
# geq-push.sh BIN DIR — one push of K homes (K clients at once) into a fresh store.
#
# push-run.sh generalised for the GEQ numbers: K concurrent `BIN push` clients
# (one per HOME in HOMES, machine names m1..mK, or MACHINE when K=1) into ONE
# serve, with the strace set widened to the barrier family plus renames, so a
# stream store's group commits can be counted exactly.
#
# Batch counting (stream): a batch is S(B1) [RR S(B2)] RM+ S(B3). Walk the
# strace in order; keep the last of {S syncfs, RR rename onto a raw frame,
# RM rename onto a raw manifest}; a syncfs whose last event was RM is a B3, so
# batches = number of those. Syncfs seen before the first RM is the boot barrier.
# HTTP (no syncfs): one batch is one PUT; its barriers are the fsyncs on frame,
# manifest and dir. APPENDS (env) is the append count of the input; unset, it is
# the puts of an HTTP run (the chunker is the same code, equiv.sh proves frames
# equal so chunk boundaries are equal).
# Prints key<TAB>value and writes the same to $OUT/geq-<name>-summary.txt, traces
# alongside. Hold the box lock: flock /var/lib/mnu/box.lock sh geq-push.sh BIN DIR
# Env: BOX, HOMES (default $BOX/plainhome), OUT ($BOX/out), PORT (7821), MACHINE
#      (plainbox), AUTHOR (uplbox), SERVE_PROCS (4), STRACE (1), APPENDS,
#      FLOOR_GB (15: refuse when free space after PROJECTED_GB would be lower),
#      PROJECTED_GB (the store size to come, default 25).
set -eu

BIN=${1:?usage: geq-push.sh BIN DIR}
DIR=${2:?usage: geq-push.sh BIN DIR}
BOX=${BOX:-/var/lib/mnu}
HOMES=${HOMES:-$BOX/plainhome}
OUT=${OUT:-$BOX/out}
PORT=${PORT:-7821}
MACHINE=${MACHINE:-plainbox}
AUTHOR=${AUTHOR:-uplbox}
SERVE_PROCS=${SERVE_PROCS:-4}
STRACE=${STRACE:-1}
FLOOR_GB=${FLOOR_GB:-15}
PROJECTED_GB=${PROJECTED_GB:-25}
NAME=$(basename "$DIR")
P=$OUT/geq-$NAME
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
for h in $HOMES; do [ -d "$h" ] || { echo "no home $h" >&2; exit 2; }; done
[ -e "$DIR" ] && { echo "$DIR exists: a push starts from a fresh store" >&2; exit 1; }
free_gb=$(df -B1G --output=avail / | tail -1 | tr -d ' ')
[ $((free_gb - PROJECTED_GB)) -ge "$FLOOR_GB" ] || { echo "not run: disk (free ${free_gb} GB, projected ${PROJECTED_GB} GB, floor ${FLOOR_GB} GB)" >&2; exit 3; }
mkdir -p "$OUT"
CFG=$P-cfg
[ -e "$CFG" ] && { echo "$CFG exists" >&2; exit 1; }
mkdir -p "$DIR" "$CFG"
chmod 700 "$CFG"
K=0
for h in $HOMES; do K=$((K + 1)); done

"$BIN" token new --author "$AUTHOR" > "$CFG/token.out"
head -1 "$CFG/token.out" > "$CFG/token"
chmod 600 "$CFG/token"
{
	echo "data_dir = \"$DIR\""
	echo "listen = \"127.0.0.1:$PORT\""
	tail -n +2 "$CFG/token.out"
} > "$CFG/server.toml"
i=0
for h in $HOMES; do
	i=$((i + 1))
	m=$MACHINE
	[ "$K" -gt 1 ] && m=m$i
	cat > "$CFG/client-$i.toml" <<CFGEOF
server = "http://127.0.0.1:$PORT"
token_file = "$CFG/token"
machine = "$m"
CFGEOF
done

: > "$P-serve.log"
export GOMAXPROCS=$SERVE_PROCS
if [ "$STRACE" = 1 ]; then
	strace -f --seccomp-bpf -y -e signal=none -e trace=fsync,fdatasync,syncfs,sync_file_range,rename,renameat,renameat2 -o "$P-strace.txt" \
		"$BIN" serve --config "$CFG/server.toml" > "$P-serve.log" 2>&1 &
else
	"$BIN" serve --config "$CFG/server.toml" > "$P-serve.log" 2>&1 &
fi
LAUNCH=$!
unset GOMAXPROCS
for _ in $(seq 1 120); do
	grep -q 'listening on' "$P-serve.log" 2>/dev/null && break
	kill -0 "$LAUNCH" 2>/dev/null || { echo "serve died at start" >&2; cat "$P-serve.log" >&2; exit 1; }
	sleep 0.5
done
if [ "$STRACE" = 1 ]; then SERVE=$(pgrep -P "$LAUNCH" | head -1); else SERVE=$LAUNCH; fi
[ -n "$SERVE" ] || { echo "no serve pid" >&2; exit 1; }
sleep 3

vmstat 1 > "$P-vmstat.txt" &
VMS=$!
START=$(date +%s.%N)
TPIDS=
i=0
for h in $HOMES; do
	i=$((i + 1))
	HOME=$h /usr/bin/time -v "$BIN" push --config "$CFG/client-$i.toml" > "$P-client-$i.out" 2> "$P-client-$i.time" &
	TPIDS="$TPIDS $!"
done
sleep 0.5
PLIST=$SERVE
for t in $TPIDS; do
	c=$(pgrep -P "$t" | head -1 || true)
	PLIST="$PLIST,${c:-$t}"
done
pidstat -u -d -h -p "$PLIST" 1 > "$P-pidstat.txt" 2>&1 &
PST=$!
RC=0
for t in $TPIDS; do wait "$t" || RC=$((RC + 1)); done
END=$(date +%s.%N)
kill "$PST" "$VMS" 2>/dev/null || true
wait "$PST" "$VMS" 2>/dev/null || true
kill -TERM "$SERVE"
for _ in $(seq 1 120); do kill -0 "$LAUNCH" 2>/dev/null || break; sleep 1; done
if kill -0 "$LAUNCH" 2>/dev/null; then echo "serve did not stop in 120 s" >&2; fi
wait "$LAUNCH" 2>/dev/null || true

SUM=$P-summary.txt
{
	printf 'run\t%s\n' "$NAME"
	printf 'bin_sha256\t%s\n' "$(sha256sum "$BIN" | cut -d' ' -f1)"
	printf 'date_pt\t%s\n' "$(TZ=America/Los_Angeles date '+%Y-%m-%d %H:%M:%S %Z')"
	printf 'clients\t%s\nhomes\t%s\n' "$K" "$HOMES"
	printf 'serve_gomaxprocs\t%s\nclient_gomaxprocs\tdefault(%s)\n' "$SERVE_PROCS" "$(nproc)"
	printf 'client_nonzero_exits\t%s\n' "$RC"
	printf 'push_wall_s\t%s\n' "$(awk -v a="$START" -v b="$END" 'BEGIN{printf "%.1f", b-a}')"
	cat "$P"-client-*.out | awk '{ for (i = 1; i < NF; i++) { k=$i; v=$(i+1); gsub(/,/, "", v); if (k=="scanned"||k=="appended"||k=="replaced"||k=="unchanged"||k=="failed"||k=="sent") s[k]+=v } }
		END { printf "client_stats\tscanned %d appended %d replaced %d unchanged %d failed %d sent %d bytes\n", s["scanned"], s["appended"], s["replaced"], s["unchanged"], s["failed"], s["sent"] }'
	printf 'client_max_rss_kb\t%s\n' "$(cat "$P"-client-*.time | sed -n 's/.*Maximum resident set size (kbytes): //p' | tr '\n' ' ')"
	printf 'client_cpu_pct\t%s\n' "$(cat "$P"-client-*.time | sed -n 's/.*Percent of CPU this job got: //p' | tr '\n' ' ')"
	printf 'client_user_s\t%s\n' "$(cat "$P"-client-*.time | sed -n 's/.*User time (seconds): //p' | tr '\n' ' ')"
	printf 'client_sys_s\t%s\n' "$(cat "$P"-client-*.time | sed -n 's/.*System time (seconds): //p' | tr '\n' ' ')"
	if [ "$STRACE" = 1 ]; then
		python3 -I - "$P-strace.txt" "$DIR/raw" "${APPENDS:-}" <<'PYEOF'
import re, sys, collections
trace, rawroot, appends = sys.argv[1:4]
by_call = collections.Counter()
role = collections.Counter()
frame = re.compile(r"^\.?[0-9a-f]{16}\.jsonl\.zst(\.\d+)?$")
man = re.compile(r"^\.?[0-9a-f]{16}\.json(\.\d+)?$")
last = None
syncfs = boot = b3 = rr_batches = 0
b2 = 0
for line in open(trace, errors="replace"):
    m = re.match(r"^(?:\d+\s+)?(\w+)\((.*)$", line)
    if not m:
        continue
    call, rest = m.groups()
    if call.startswith("rename"):
        qs = re.findall(r'"((?:[^"\\]|\\.)*)"', rest)
        if not qs:
            continue
        tgt = qs[-1]
        by_call[call] += 1
        if not tgt.startswith(rawroot) and "/raw/" not in tgt:
            continue
        base = tgt.rsplit("/", 1)[-1]
        if man.match(base) and not base.startswith("."):
            last = "RM"
        elif frame.match(base) and not base.startswith("."):
            last = "RR"
        continue
    pm = re.match(r"(\d+)<([^>]*)>", rest)
    path = pm.group(2) if pm else ""
    by_call[call] += 1
    if call == "syncfs":
        syncfs += 1
        if last == "RM":
            b3 += 1
        elif last == "RR":
            b2 += 1
        elif b3 == 0 and last is None:
            boot += 1
        last = "S"
        continue
    if not path.startswith(rawroot):
        role["other"] += 1
        continue
    base = path.rsplit("/", 1)[-1]
    if man.match(base):
        role["manifest"] += 1
    elif frame.match(base):
        role["frame_replace" if base.startswith(".") else "frame_append"] += 1
    elif "harness=" in base or base.startswith(("author=", "machine=")):
        role["dir"] += 1
    else:
        role["unclassified"] += 1
for k in sorted(by_call):
    print("trace_%s\t%d" % (k, by_call[k]))
for k in ("frame_replace", "frame_append", "manifest", "dir", "other", "unclassified"):
    print("fsync_role_%s\t%d" % (k, role[k]))
fs_store = role["frame_replace"] + role["frame_append"] + role["manifest"] + role["dir"]
if syncfs:
    print("syncfs_total\t%d" % syncfs)
    print("syncfs_boot\t%d" % boot)
    print("batches\t%d" % b3)
    print("batches_with_B2\t%d" % b2)
    barriers = syncfs - boot + fs_store
    n_batches = b3
    print("syncfs_per_batch\t%.3f" % ((syncfs - boot) / b3 if b3 else 0))
else:
    barriers = fs_store
    n_batches = role["manifest"]
    print("puts\t%d" % role["manifest"])
    print("puts_replace\t%d" % role["frame_replace"])
    print("puts_append\t%d" % role["frame_append"])
print("store_barriers\t%d" % barriers)
print("batches_all\t%d" % n_batches)
if not appends and not syncfs:
    appends = str(role["manifest"])
if appends:
    a = int(appends)
    print("appends\t%d" % a)
    print("appends_per_batch\t%.3f" % (a / n_batches if n_batches else 0))
    print("barriers_per_append\t%.3f" % (barriers / a if a else 0))
else:
    print("appends_per_batch\tneeds APPENDS")
PYEOF
	fi
	printf 'err_client_failed\t%s\n' "$(cat "$P"-client-*.out | sed -n 's/.*failed \([0-9]*\).*/\1/p' | tr '\n' ' ')"
	printf 'err_cycle_err_not_nil\t%s\n' "$(grep 'cycle .* err=' "$P-serve.log" | grep -vc 'err=<nil>' || true)"
	printf 'err_serve_panic_fatal\t%s\n' "$(grep -ci 'panic\|fatal' "$P-serve.log" || true)"
	printf 'cycles_logged\t%s\n' "$(grep -c 'cycle ' "$P-serve.log" || true)"
	printf 'store_files\t%s\n' "$(find "$DIR/raw" -type f | wc -l)"
	printf 'store_sources\t%s\n' "$(find "$DIR/raw" -type f -name '*.json' ! -name '.*' | wc -l)"
	printf 'store_raw_bytes\t%s\n' "$(du -sb "$DIR/raw" | cut -f1)"
	printf 'store_dotfiles\t%s\n' "$(find "$DIR/raw" -type f -name '.*' | wc -l)"
} > "$SUM"
grep 'cycle ' "$P-serve.log" | cut -c1-300 > "$P-cycles.txt" || true
cat "$SUM"
