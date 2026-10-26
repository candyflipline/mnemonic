#!/bin/sh
# push-run.sh BIN DIR — one full push of the plain HOME into a fresh store.
#
# Makes DIR (must not exist) as a serve data_dir, starts `BIN serve` on it
# under `strace -f` (fsync family only, fd paths with -y; --seccomp-bpf so only
# those syscalls stop), runs ONE `BIN push` of the whole plain HOME with the
# clock running, stops serve. The store is DIR/raw. Prints (key<TAB>value) and
# writes the same plus raw traces to $OUT/base-<name>-*:
#   push_wall_s       client wall, process start to exit
#   client_stats      the client's own line (scanned/appended/replaced/failed/bytes)
#   puts              server PUTs that wrote a manifest (one manifest fsync each)
#   fsync_*           counts by syscall, then by store role (frame replace, frame
#                     append, manifest, dir, other = index files outside DIR/raw)
#   fsync_per_put     (frame + manifest + dir fsyncs) / puts
#   err_*             client failed count, cycle lines with err!=nil, serve.log
#                     lines naming panic or fatal
# strace -c is not used: -c prints only per-syscall totals, and the roles above
# need the fd path. The per-syscall totals are the same lines counted by name.
# Alongside: pidstat -u -d on serve and push (1 s), vmstat 1, /usr/bin/time -v
# on the client. Hold the box lock for the whole run:
#   flock /var/lib/mnu/box.lock sh push-run.sh BIN DIR
# Env: BOX (/var/lib/mnu), PH ($BOX/plainhome), OUT ($BOX/out), PORT (7791),
#      QUIESCE (0; 1 waits, after the timing, for a cycle that changed nothing before stopping serve),
#      MACHINE (plainbox), AUTHOR (uplbox), SERVE_PROCS (4: serve GOMAXPROCS),
#      CLIENT_PROCS (unset: the Go default, every core), STRACE (1; 0 runs serve bare).
set -eu

BIN=${1:?usage: push-run.sh BIN DIR}
DIR=${2:?usage: push-run.sh BIN DIR}
BOX=${BOX:-/var/lib/mnu}
PH=${PH:-$BOX/plainhome}
OUT=${OUT:-$BOX/out}
PORT=${PORT:-7791}
MACHINE=${MACHINE:-plainbox}
AUTHOR=${AUTHOR:-uplbox}
SERVE_PROCS=${SERVE_PROCS:-4}
STRACE=${STRACE:-1}
NAME=$(basename "$DIR")
P=$OUT/base-$NAME
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
[ -d "$PH" ] || { echo "no plain HOME at $PH: run plainhome.sh" >&2; exit 2; }
[ -e "$DIR" ] && { echo "$DIR exists: a push-run starts from a fresh store" >&2; exit 1; }
mkdir -p "$OUT"
CFG=$P-cfg
[ -e "$CFG" ] && { echo "$CFG exists" >&2; exit 1; }
mkdir -p "$DIR" "$CFG"
chmod 700 "$CFG"

"$BIN" token new --author "$AUTHOR" > "$CFG/token.out"
head -1 "$CFG/token.out" > "$CFG/token"
chmod 600 "$CFG/token"
{
	echo "data_dir = \"$DIR\""
	echo "listen = \"127.0.0.1:$PORT\""
	echo "push_listen = \"127.0.0.1:$((PORT + 1000))\""
	tail -n +2 "$CFG/token.out"
} > "$CFG/server.toml"
cat > "$CFG/client.toml" <<CFGEOF
server = "http://127.0.0.1:$((PORT + 1000))"
token_file = "$CFG/token"
machine = "$MACHINE"
CFGEOF

: > "$P-serve.log"
export GOMAXPROCS=$SERVE_PROCS
if [ "$STRACE" = 1 ]; then
	strace -f --seccomp-bpf -y -e signal=none -e trace=fsync,fdatasync,syncfs,sync_file_range -o "$P-strace.txt" \
		"$BIN" serve --config "$CFG/server.toml" > "$P-serve.log" 2>&1 &
else
	"$BIN" serve --config "$CFG/server.toml" > "$P-serve.log" 2>&1 &
fi
LAUNCH=$!
unset GOMAXPROCS
[ -z "${CLIENT_PROCS:-}" ] || export GOMAXPROCS=$CLIENT_PROCS

for _ in $(seq 1 600); do
	grep -q 'push serving' "$P-serve.log" 2>/dev/null && break
	kill -0 "$LAUNCH" 2>/dev/null || { echo "serve died at start" >&2; cat "$P-serve.log" >&2; exit 1; }
	sleep 0.5
done
if [ "$STRACE" = 1 ]; then SERVE=$(pgrep -P "$LAUNCH" | head -1); else SERVE=$LAUNCH; fi
[ -n "$SERVE" ] || { echo "no serve pid" >&2; exit 1; }
# the boot cycle on an empty store finishes before the clock starts
sleep 3

vmstat 1 > "$P-vmstat.txt" &
VMS=$!
START=$(date +%s.%N)
HOME=$PH /usr/bin/time -v "$BIN" push --config "$CFG/client.toml" > "$P-client.out" 2> "$P-client.time" &
TPID=$!
sleep 0.5
CLIENT=$(pgrep -P "$TPID" | head -1)
pidstat -u -d -h -p "$SERVE,${CLIENT:-$TPID}" 1 > "$P-pidstat.txt" 2>&1 &
PST=$!
RC=0
wait "$TPID" || RC=$?
END=$(date +%s.%N)
kill "$PST" "$VMS" 2>/dev/null || true
wait "$PST" "$VMS" 2>/dev/null || true
if [ "${QUIESCE:-0}" = 1 ]; then
	# after the clock stopped: let serve index everything and stop on a clean
	# shutdown, so a later serve on this store boots from a settled index
	for _ in $(seq 1 360); do
		last=$(grep 'cycle ' "$P-serve.log" | tail -1)
		case $last in *"resumed=0 fresh=0 reparsed=0"*"stale=0;"*) break ;; esac
		sleep 5
	done
fi
kill -TERM "$SERVE"
for _ in $(seq 1 120); do kill -0 "$LAUNCH" 2>/dev/null || break; sleep 1; done
if kill -0 "$LAUNCH" 2>/dev/null; then echo "serve did not stop in 120 s" >&2; fi
wait "$LAUNCH" 2>/dev/null || true

SUM=$P-summary.txt
{
	printf 'run\t%s\n' "$NAME"
	printf 'bin_sha256\t%s\n' "$(sha256sum "$BIN" | cut -d' ' -f1)"
	printf 'date_pt\t%s\n' "$(TZ=America/Los_Angeles date '+%Y-%m-%d %H:%M:%S %Z')"
	printf 'serve_gomaxprocs\t%s\nclient_gomaxprocs\t%s\n' "$SERVE_PROCS" "${CLIENT_PROCS:-default($(nproc))}"
	printf 'strace\t%s\n' "$STRACE"
	printf 'client_exit\t%s\n' "$RC"
	printf 'push_wall_s\t%s\n' "$(awk -v a="$START" -v b="$END" 'BEGIN{printf "%.1f", b-a}')"
	printf 'client_stats\t%s\n' "$(cat "$P-client.out")"
	printf 'client_max_rss_kb\t%s\n' "$(sed -n 's/.*Maximum resident set size (kbytes): //p' "$P-client.time")"
	printf 'client_user_s\t%s\nclient_sys_s\t%s\n' "$(sed -n 's/.*User time (seconds): //p' "$P-client.time")" "$(sed -n 's/.*System time (seconds): //p' "$P-client.time")"
	printf 'client_cpu_pct\t%s\n' "$(sed -n 's/.*Percent of CPU this job got: //p' "$P-client.time")"
	if [ "$STRACE" = 1 ]; then
		python3 -I - "$P-strace.txt" "$DIR/raw" <<'PYEOF'
import re, sys, collections
trace, rawroot = sys.argv[1:3]
by_call = collections.Counter()
role = collections.Counter()
frame = re.compile(r"^\.?[0-9a-f]{16}\.jsonl\.zst(\.\d+)?$")
man = re.compile(r"^\.?[0-9a-f]{16}\.json(\.\d+)?$")
for line in open(trace, errors="replace"):
    m = re.match(r"^(?:\d+\s+)?(\w+)\((\d+)<([^>]*)>", line)
    if not m:
        continue
    call, _, path = m.groups()
    by_call[call] += 1
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
    print("fsync_%s\t%d" % (k, by_call[k]))
for k in ("frame_replace", "frame_append", "manifest", "dir", "other", "unclassified"):
    print("fsync_role_%s\t%d" % (k, role[k]))
puts = role["manifest"]
store = role["frame_replace"] + role["frame_append"] + role["manifest"] + role["dir"]
print("puts\t%d" % puts)
print("fsync_store_total\t%d" % store)
print("fsync_per_put\t%.3f" % (store / puts if puts else 0))
print("puts_replace\t%d" % role["frame_replace"])
print("puts_append\t%d" % role["frame_append"])
PYEOF
	fi
	printf 'err_client_failed\t%s\n' "$(sed -n 's/.*failed \([0-9]*\).*/\1/p' "$P-client.out")"
	printf 'err_cycle_err_not_nil\t%s\n' "$(grep 'cycle .* err=' "$P-serve.log" | grep -vc 'err=<nil>' || true)"
	printf 'err_serve_panic_fatal\t%s\n' "$(grep -ci 'panic\|fatal' "$P-serve.log" || true)"
	printf 'cycles_logged\t%s\n' "$(grep -c 'cycle ' "$P-serve.log" || true)"
	printf 'store_files\t%s\n' "$(find "$DIR/raw" -type f | wc -l)"
	printf 'store_sources\t%s\n' "$(find "$DIR/raw" -type f -name '*.json' ! -name '.*' | wc -l)"
	printf 'store_bytes\t%s\n' "$(du -sb "$DIR/raw" | cut -f1)"
	printf 'store_dotfiles\t%s\n' "$(find "$DIR/raw" -type f -name '.*' | wc -l)"
} > "$SUM"
grep 'cycle ' "$P-serve.log" | cut -c1-300 > "$P-cycles.txt" || true
cat "$SUM"
