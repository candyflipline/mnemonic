#!/bin/sh
# crash.sh BIN — the uploader's crash-kill gate (docs/design/uploader.md
# "Crash table"). Prints one row per kill point and exits 0 iff every row
# converged with no acked append missing, and the serial ack check found no
# ack before its batch's last barrier returned.
#
# Server kill points: serve runs under strace -f with delay_enter injected on
# the target syscall, and `crashdrv killat` SIGKILLs serve while a thread is
# held at that call's entry. strace's own inject when=N counts per thread on
# strace 6.8 (checked on the box), and the committer moves between threads,
# so the point is counted by killat from the strace log (piped: strace fully
# buffers -o file, line-buffers a pipe), globally, as "the
# first target call after these completed calls since the last syncfs":
#   S1 row 2  syncfs after an extend's ftruncate of a raw (B1), seeded from
#             R1 after the HOME copy's 5 next-smallest files got a line
#             appended (a fresh push folds a big file's Extends into its
#             Replace's batch, so it never extends a linked raw)
#   S2 row 6  manifest rename after one manifest rename  (W2b, mid batch)
#   S3 row 7  syncfs after a manifest rename             (B3)
#   S4 row 5  syncfs after a raw rename (B2), seeded from R1 after the HOME
#             copy's smallest file got a line prepended, so it is the one
#             rewrite and W1 unlinked its manifest (man_unlinks in the proof)
# S1 and S4 converge against R2 (R1 plus one push of the changed HOME).
# Client kill points: the push client talks to `crashdrv proxy`, which holds
# the stream at the point, creates a marker, and crash.sh SIGKILLs the client:
#   C1 row 9  after Welcome page 1 of 2 (a synthesized HOME of TINY_N files)
#   C2 row 9  acks received from serve and not forwarded (appends sent)
#   C3 row 9  serial; Append k+1 held after Ack k reached the client
# Every point then restarts serve on the same data dir, reads its Welcome
# (oracle b: every APPLIED ack the proxy saw has size <= Welcome's), runs one
# push to completion, and equiv.sh's the store against an uninterrupted push
# of the same HOME from the same seed (oracle a). SER is one serial push with
# serve under strace -ttt -T (oracle c, crashdrv ackcheck).
#
#   flock /var/lib/mnu/box.lock sh crash.sh BIN
# Env: BOX (/var/lib/mnu), DRV (crashdrv next to BIN), SMALL ($BOX/smallhome,
#      read only, copied), RUN ($BOX/gcrash/run-<time>, must not exist),
#      PORT (7891, listen), PUSH_PORT (7892, push_listen), PPORT (7893),
#      TINY_N (5000), DELAY_US (100000),
#      FREEFLOOR_GB (15): refuse to copy when / has less free.
# Writes RUN/rows.tsv and per point RUN/<point>.* (logs, strace, counts).
set -eu

BIN=${1:?usage: crash.sh BIN}
BOX=${BOX:-/var/lib/mnu}
DRV=${DRV:-$(dirname "$BIN")/crashdrv}
SMALL=${SMALL:-$BOX/smallhome}
RUN=${RUN:-$BOX/gcrash/run-$(date +%Y%m%d-%H%M%S)}
PORT=${PORT:-7891}
PUSH_PORT=${PUSH_PORT:-7892}
PPORT=${PPORT:-7893}
TINY_N=${TINY_N:-5000}
DELAY_US=${DELAY_US:-100000}
FREEFLOOR_GB=${FREEFLOOR_GB:-15}
HERE=$(cd "$(dirname "$0")" && pwd)
[ -x "$BIN" ] && [ -x "$DRV" ] || { echo "need executable $BIN and $DRV" >&2; exit 2; }
[ -e "$RUN" ] && { echo "$RUN exists" >&2; exit 1; }
mkdir -p "$RUN/cfg"
chmod 700 "$RUN/cfg"
ROWS=$RUN/rows.tsv
FAILS=0
LAUNCH= SPID= KPID= PXPID= CPID=
ppid_of() { awk '{print $4}' "/proc/$1/stat" 2>/dev/null; }
cleanup() {
	for p in $SPID; do [ "$(ppid_of "$p")" = "$LAUNCH" ] && kill -9 "$p" 2>/dev/null; done
	for p in $CPID $PXPID $KPID $LAUNCH; do [ "$(ppid_of "$p")" = $$ ] && kill -9 "$p" 2>/dev/null; done
	return 0
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$BIN" token new --author gcrash > "$RUN/cfg/token.out"
head -1 "$RUN/cfg/token.out" > "$RUN/cfg/token"
tail -n +2 "$RUN/cfg/token.out" > "$RUN/cfg/tokens.toml"
chmod 600 "$RUN/cfg/token"
client_cfg() { # name port machine
	printf 'server = "http://127.0.0.1:%s"\ntoken_file = "%s"\nmachine = "%s"\n' "$2" "$RUN/cfg/token" "$3" > "$RUN/cfg/$1.toml"
}
client_cfg small "$PUSH_PORT" gcrash
client_cfg small-proxy "$PPORT" gcrash
client_cfg tiny "$PUSH_PORT" gcrash-tiny
client_cfg tiny-proxy "$PPORT" gcrash-tiny

now() { date +%s.%N; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.1f", b-a}'; }

# serve_start DIR TAG [strace args...]: sets LAUNCH and SPID
serve_start() {
	d=$1 tag=$2
	shift 2
	{
		echo "data_dir = \"$d\""
		echo "listen = \"127.0.0.1:$PORT\""
		echo "push_listen = \"127.0.0.1:$PUSH_PORT\""
		cat "$RUN/cfg/tokens.toml"
	} > "$d.$tag.toml"
	if [ $# -gt 0 ]; then
		strace "$@" "$BIN" serve --config "$d.$tag.toml" > "$d.$tag.serve.log" 2>&1 &
	else
		"$BIN" serve --config "$d.$tag.toml" > "$d.$tag.serve.log" 2>&1 &
	fi
	LAUNCH=$!
	i=0
	until grep -q 'mnemonic serve: HTTP' "$d.$tag.serve.log" 2>/dev/null; do
		kill -0 "$LAUNCH" 2>/dev/null || { echo "serve died at start ($d $tag)" >&2; return 1; }
		i=$((i + 1))
		[ $i -lt 600 ] || { echo "serve not listening after 120 s" >&2; return 1; }
		sleep 0.2
	done
	if [ $# -gt 0 ]; then SPID=$(pgrep -P "$LAUNCH" -x mnemonic); else SPID=$LAUNCH; fi
}

serve_stop() {
	kill -TERM "$SPID" 2>/dev/null || true
	i=0
	while kill -0 "$LAUNCH" 2>/dev/null && [ $i -lt 600 ]; do sleep 0.2; i=$((i + 1)); done
	wait "$LAUNCH" 2>/dev/null || true
}

proxy_start() { # log args...
	plog=$1
	shift
	"$DRV" proxy -listen "127.0.0.1:$PPORT" -upstream "127.0.0.1:$PUSH_PORT" -log "$plog" "$@" 2> "$plog.err" &
	PXPID=$!
	i=0
	until ss -ltn "sport = :$PPORT" | grep -q LISTEN; do
		i=$((i + 1))
		[ $i -lt 100 ] || { echo "proxy not listening" >&2; return 1; }
		sleep 0.1
	done
}

proxy_stop() {
	kill "$PXPID" 2>/dev/null || true
	wait "$PXPID" 2>/dev/null || true
}

push_bg() { # home cfg log
	HOME=$1 "$BIN" push --config "$RUN/cfg/$2.toml" > "$3" 2>&1 &
	CPID=$!
}

wait_pid() { # pid seconds: 0 if it exited in time
	i=0
	while kill -0 "$1" 2>/dev/null; do
		i=$((i + 1))
		[ $i -lt $(($2 * 5)) ] || return 1
		sleep 0.2
	done
}

seed() { # dir seed|-
	free_gb=$(df -B1G --output=avail / | tail -1 | tr -d ' ')
	[ "$free_gb" -ge "$FREEFLOOR_GB" ] || { echo "free ${free_gb} GB is under the ${FREEFLOOR_GB} GB floor" >&2; exit 1; }
	if [ "$2" = - ]; then mkdir "$1"; else cp -a "$2" "$1"; fi
}

reference() { # name home cfg seed|-
	d=$RUN/$1
	seed "$d" "$4"
	serve_start "$d" ref
	t0=$(now)
	rc=0
	HOME=$2 "$BIN" push --config "$RUN/cfg/$3.toml" > "$d.ref.push.out" 2>&1 || rc=$?
	t1=$(now)
	serve_stop
	printf 'ref\t%s\tpush_rc=%s\tpush_s=%s\t%s\n' "$1" "$rc" "$(secs "$t0" "$t1")" "$(tail -1 "$d.ref.push.out")" | tee -a "$RUN/refs.tsv"
	[ "$rc" = 0 ] || { echo "reference push $1 failed" >&2; exit 1; }
}

acked_check() { # proxylog welcome -> "acked N missing M"
	python3 -I - "$1" "$2" <<'PYEOF'
import sys
acked = {}
n = 0
for line in open(sys.argv[1]):
    x = line.rstrip("\n").split("\t")
    if len(x) >= 6 and x[1] == "ack" and x[3] == "APPLIED":
        n += 1
        acked[x[4]] = max(acked.get(x[4], 0), int(x[5]))
have = {}
for line in open(sys.argv[2]):
    s, z = line.split("\t")
    have[s] = int(z)
missing = sum(1 for s, z in acked.items() if have.get(s, -1) < z)
print("acked=%d srcs=%d missing=%d" % (n, len(acked), missing))
sys.exit(1 if missing else 0)
PYEOF
}

# converge POINT ROW DIR HOME CFG MACHINE REF PROOF
converge() {
	p=$1 row=$2 d=$3 home=$4 cfg=$5 mach=$6 ref=$7 proof=$8
	t0=$(now)
	serve_start "$d" restart
	"$DRV" welcome -addr "127.0.0.1:$PUSH_PORT" -token-file "$RUN/cfg/token" -machine "$mach" > "$d.welcome" 2> "$d.welcome.err" || true
	ack=$(acked_check "$d.proxy" "$d.welcome") && ackrc=0 || ackrc=$?
	prc=0
	HOME=$home "$BIN" push --config "$RUN/cfg/$cfg.toml" > "$d.converge.push.out" 2>&1 || prc=$?
	t1=$(now)
	serve_stop
	erc=0
	sh "$HERE/equiv.sh" "$ref" "$d" > "$d.equiv" 2>&1 || erc=$?
	ok=PASS
	[ "$erc" = 0 ] && [ "$ackrc" = 0 ] && [ "$prc" = 0 ] || { ok=FAIL; FAILS=$((FAILS + 1)); }
	printf '%s\trow%s\t%s\tequiv_rc=%s\t%s\tconverge_push_rc=%s\tconverge_s=%s\t%s\n' \
		"$p" "$row" "$proof" "$erc" "$ack" "$prc" "$(secs "$t0" "$t1")" "$ok" | tee -a "$ROWS"
}

# server_point POINT ROW HOME CFG MACHINE SEED REF INJECT_SYS KILLAT_ARGS...
server_point() {
	p=$1 row=$2 home=$3 cfg=$4 mach=$5 sd=$6 ref=$7 inj=$8
	shift 8
	d=$RUN/$p
	seed "$d" "$sd"
	serve_start "$d" kill -f -qq -ttt -T -y -s 4096 -o "|cat > $d.strace" \
		-e signal=none -e trace=syncfs,rename,renameat,renameat2,ftruncate,unlinkat \
		-e "inject=$inj:delay_enter=$DELAY_US"
	"$DRV" killat -pid "$SPID" -strace "$d.strace" "$@" > "$d.killat" 2>&1 &
	KPID=$!
	proxy_start "$d.proxy"
	push_bg "$home" "$cfg-proxy" "$d.kill.push.out"
	while kill -0 "$KPID" 2>/dev/null && kill -0 "$CPID" 2>/dev/null; do sleep 0.2; done
	wait_pid "$KPID" 15 || kill "$KPID" 2>/dev/null || true
	krc=0
	wait "$KPID" || krc=$?
	wait_pid "$LAUNCH" 60 || true
	wait "$LAUNCH" 2>/dev/null || true
	wait_pid "$CPID" 120 || kill -9 "$CPID" 2>/dev/null || true
	wait "$CPID" 2>/dev/null || true
	proxy_stop
	if [ "$krc" != 0 ]; then
		printf '%s\trow%s\tkill point not reached: %s\tFAIL\n' "$p" "$row" "$(tail -1 "$d.killat")" | tee -a "$ROWS"
		FAILS=$((FAILS + 1))
		serve_stop
		return 0
	fi
	tid=$(sed -n 's/.*tid=\([0-9]*\).*/\1/p' "$d.killat")
	tail -n 12 "$d.strace" > "$d.strace-tail"
	short='s#^[0-9]* [0-9.]* ##; s#AT_FDCWD<[^>]*>#AT_FDCWD#g; s#"[^"]*/\([^"/]*\)"#"\1"#g; s#<[^>]*/\([^/>]*\)>#<\1>#g; s# <unfinished \.\.\.>##'
	entry=$(grep "^$tid " "$d.strace" | grep -v -e '+++ killed' -e ' resumed>' | tail -1 | sed "$short")
	last=$(grep "^$tid " "$d.strace" | grep -v '+++ killed' | tail -1)
	case $last in *'resumed>) = ?') entry="$entry) = ?" ;; *'= ?') ;; *) entry="$entry NOT-IN-CALL" ;; esac
	awk '/ <unfinished \.\.\.>$/ { sub(/ <unfinished \.\.\.>$/, ""); p[$1] = $0; next }
		/<\.\.\. [a-z0-9_]+ resumed>/ && ($1 in p) { r = $0; sub(/^.*resumed>/, "", r); print p[$1] r; delete p[$1]; next }
		{ print }' "$d.strace" > "$d.strace-joined"
	j=$d.strace-joined
	proof="$entry; before it: syncfs_done=$(grep -cE 'syncfs\(.*= 0' "$j" || true)"
	proof="$proof man_renames=$(grep -cE "rename.*$MAN" "$j" || true)"
	proof="$proof raw_renames=$(grep -cE "rename.*$RAWRN" "$j" || true)"
	proof="$proof raw_extend_truncs=$(grep -cE "$EXT" "$j" || true)"
	proof="$proof man_unlinks=$(grep -cE "$MANUNLINK" "$j" || true)"
	case $entry in *NOT-IN-CALL) FAILS=$((FAILS + 1)) ;; esac
	converge "$p" "$row" "$d" "$home" "$cfg" "$mach" "$ref" "$proof"
}

# client_point POINT HOME CFG MACHINE SEED REF PROXY_ARGS...
client_point() {
	p=$1 home=$2 cfg=$3 mach=$4 sd=$5 ref=$6
	shift 6
	d=$RUN/$p
	seed "$d" "$sd"
	serve_start "$d" kill
	proxy_start "$d.proxy" -marker "$d.marker" "$@"
	push_bg "$home" "$cfg-proxy" "$d.kill.push.out"
	i=0
	until [ -e "$d.marker" ]; do
		i=$((i + 1))
		if [ $i -ge 3000 ] || ! kill -0 "$CPID" 2>/dev/null; then break; fi
		sleep 0.1
	done
	if [ -e "$d.marker" ] && kill -9 "$CPID" 2>/dev/null; then
		wait "$CPID" 2>/dev/null || true
		proof="client SIGKILL at $(grep -P '\thold\t' "$d.proxy" | cut -f3- | tr '\t' ' '); appends_fwd=$(grep -cP '\tappend\t' "$d.proxy" || true) acks_from_serve=$(grep -cP '\tack\t' "$d.proxy" || true)"
	else
		wait "$CPID" 2>/dev/null || true
		printf '%s\trow9\tkill point not reached (marker %s)\tFAIL\n' "$p" "$([ -e "$d.marker" ] && echo yes || echo no)" | tee -a "$ROWS"
		FAILS=$((FAILS + 1))
		proxy_stop
		serve_stop
		return 0
	fi
	proxy_stop
	serve_stop
	converge "$p" 9 "$d" "$home" "$cfg" "$mach" "$ref" "$proof"
}

echo "run $RUN bin $(sha256sum "$BIN" | cut -c1-16) drv $(sha256sum "$DRV" | cut -c1-16) $(TZ=America/Los_Angeles date '+%Y-%m-%d %H:%M:%S %Z')"
H=$RUN/home
cp -a "$SMALL" "$H"
printf 'home_files\t%s\n' "$(find "$H" -type f | wc -l)"

MAN='/raw/[^"]*/[0-9a-f]{16}\.json"\) = 0'
RAWRN='/raw/[^"]*/[0-9a-f]{16}\.jsonl\.zst"\) = 0'
EXT='ftruncate\([0-9]+</[^>]*/raw/[^>]*\.jsonl\.zst>, [1-9]'
MANUNLINK='unlinkat\(AT_FDCWD[^,]*, "[^"]*/raw/[^"]*/[0-9a-f]{16}\.json", 0\) = 0'
reference R1 "$H" small -
server_point S2 6 "$H" small gcrash - "$RUN/R1" rename,renameat,renameat2 -sys rename -timeout 300s -target '/raw/.*/[0-9a-f]{16}\.json$' -need "$MAN"
server_point S3 7 "$H" small gcrash - "$RUN/R1" syncfs -sys syncfs -timeout 300s -need "$MAN"
client_point C2 "$H" small gcrash - "$RUN/R1" -hold acks -k 5
client_point C3 "$H" small gcrash - "$RUN/R1" -serial -hold append -k 5

d=$RUN/SER
mkdir "$d"
serve_start "$d" ser -f -qq -ttt -T -y -s 4096 -o "$d.strace" -e signal=none -e trace=syncfs,rename,renameat,renameat2
proxy_start "$d.proxy" -serial
src=0
HOME=$H "$BIN" push --config "$RUN/cfg/small-proxy.toml" > "$d.push.out" 2>&1 || src=$?
proxy_stop
serve_stop
crc=0
"$DRV" ackcheck -strace "$d.strace" -proxy "$d.proxy" > "$d.ackcheck" 2>&1 || crc=$?
erc=0
sh "$HERE/equiv.sh" "$RUN/R1" "$d" > "$d.equiv" 2>&1 || erc=$?
[ "$crc" = 0 ] && [ "$src" = 0 ] && [ "$erc" = 0 ] && ok=PASS || { ok=FAIL; FAILS=$((FAILS + 1)); }
printf 'SER\tack-after-B3\t%s\tpush_rc=%s\tequiv_rc=%s\t%s\n' "$(tr '\t' '=' < "$d.ackcheck" | tr '\n' ' ' | sed 's/ $//')" "$src" "$erc" "$ok" | tee -a "$ROWS"

python3 -I - "$H" <<'PYEOF'
import os, sys
sizes = []
for d, _, fs in os.walk(os.path.join(sys.argv[1], ".claude", "projects")):
    for f in fs:
        p = os.path.join(d, f)
        if f.endswith(".jsonl") and os.path.getsize(p) > 0:
            sizes.append((os.path.getsize(p), p))
best = min(sizes)
z, p = best
with open(p, "rb") as f:
    old = f.read()
with open(p, "wb") as f:
    f.write(b'{"type":"gcrash-rewrite"}\n' + old)
print("rewrote\t1")
for _, q in sorted(sizes)[1:6]:
    with open(q, "ab") as f:
        f.write(b'{"type":"gcrash-extend"}\n')
print("extended\t5")
PYEOF
reference R2 "$H" small "$RUN/R1"
server_point S1 2 "$H" small gcrash "$RUN/R1" "$RUN/R2" syncfs -sys syncfs -timeout 300s -need "$EXT"
server_point S4 5 "$H" small gcrash "$RUN/R1" "$RUN/R2" syncfs -sys syncfs -timeout 300s -need "$RAWRN"

T=$RUN/tiny
tiny_gen() { # phase a: TINY_N one-line files; phase b: append a line to 100, add 20
	python3 -I - "$T" "$TINY_N" "$1" <<'PYEOF'
import os, sys
root, n, phase = sys.argv[1], int(sys.argv[2]), sys.argv[3]
def path(i):
    return os.path.join(root, ".claude", "projects", "gcrash-%02d" % (i % 50), "%08x-0000-4000-8000-%012x.jsonl" % (i, i))
def write(i, mode, kind):
    os.makedirs(os.path.dirname(path(i)), exist_ok=True)
    with open(path(i), mode) as f:
        f.write('{"type":"%s","gcrash":%d}\n' % (kind, i))
if phase == "a":
    for i in range(n):
        write(i, "w", "user")
else:
    for i in range(0, n, n // 100):
        write(i, "a", "assistant")
    for i in range(n, n + 20):
        write(i, "w", "user")
print("tiny_%s\t%d" % (phase, sum(len(fs) for _, _, fs in os.walk(root))))
PYEOF
}
tiny_gen a
reference TA "$T" tiny -
tiny_gen b
reference TB "$T" tiny "$RUN/TA"
client_point C1 "$T" tiny gcrash-tiny "$RUN/TA" "$RUN/TB" -hold welcome2

echo "rows $(wc -l < "$ROWS") fails $FAILS"
[ "$FAILS" = 0 ]
