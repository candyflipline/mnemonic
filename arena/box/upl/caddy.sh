#!/bin/sh
# caddy.sh BIN — the push stream through the Caddy prod runs, one row per check.
#
# Starts two `BIN serve` processes (C on the docker0 gateway 172.17.0.1, where
# the container's host.docker.internal lands: HTTP listen 8740 and the native
# gRPC push_listen 8741 behind Caddy's @push route; D on 127.0.0.1 listen 8744
# and push_listen 8745 as the no-Caddy control) and a container of
# caddy:2.10.2-alpine with the repo's
# ops/Caddyfile mounted read-only (MORAINE_HOST=localhost:8443, so Caddy
# serves its internal CA's certificate). Clients verify that certificate:
# SSL_CERT_FILE names the system bundle plus Caddy's root, set on the client
# processes only, never the box's trust store and never a verify switch.
#
# Checks, all with the Caddyfile as committed (flush_interval -1):
#   converge   product client pushes a synthesized tree through Caddy; frames
#              equal a direct push; a second push finds nothing to send
#   append32   first line 32 MiB less 5 bytes: acked, frame sha equals direct
#   stream300  caddydrv big: 330 MiB in 11 appends on one stream, acks = appends
#   idle       on a fresh Caddy (tcpdump already running), push --watch idles
#              IDLE_S s, then one line is acked within 5 s with no new stream
#              (uprobe on intake Sync entry); the capture of Caddy to 8741
#              (docker0, plaintext h2c) is parsed into HTTP/2 PING / GOAWAY /
#              RST_STREAM frames per direction, and the client to Caddy leg
#              (lo:8443, TLS) into 34-byte application-data records (a PING
#              frame, 9 + 8 bytes, plus the TLS 1.3 type byte and tag)
#   separate   8741 is bound only on the docker0 gateway; the HTTP listener
#              answers no Push path; Caddy's non-push route cannot reach it
# Then ROUNDS alternations of three arms (neg1 = committed file, default = the
# same file with the flush_interval line removed, direct = no Caddy), each:
#   latency    LATN appends one at a time, client send-to-ack
#   cancel     caddydrv hold killed with SIGKILL; kill-to-backend-handler-end
# Backend handler end is a uprobe on intake.(*service).leave, the deferred call
# of Sync, read from tracefs with the CLOCK_MONOTONIC trace clock; the kill
# time is read from the same clock the instant before kill(2).
#
# Rows go to stdout and $R/rows.tsv: row<TAB>check<TAB>setting<TAB>numbers<TAB>PASS|FAIL
#   flock /var/lib/mnu/box.lock sh caddy.sh /var/lib/mnu/fgcaddy-bin/mnemonic
# Env: BOX, W (work dir), CADDYFILE, DRV (caddydrv binary), IDLE_S (700),
#      ROUNDS (5), LATN (20), KILLCAP (60 s to wait for a handler end).
set -u

BIN=${1:?usage: caddy.sh BIN}
BOX=${BOX:-/var/lib/mnu}
HERE=$(cd "$(dirname "$0")" && pwd)
CADDYFILE=${CADDYFILE:-$HERE/../../../ops/Caddyfile}
DRV=${DRV:-$BOX/fgcaddy-bin/caddydrv}
W=${W:-$BOX/fgcaddy}
IDLE_S=${IDLE_S:-700}
ROUNDS=${ROUNDS:-5}
LATN=${LATN:-20}
KILLCAP=${KILLCAP:-60}
IMG=caddy:2.10.2-alpine
CNAME=fgcaddy-caddy
CPORT=8443
SPORT=8740
PPORT=8741
DPORT=8744
DPUSH=8745
TR=/sys/kernel/tracing
GO=/usr/local/go/bin/go

R=$W/run-$(TZ=America/Los_Angeles date +%Y%m%d-%H%M%S)
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
[ -x "$DRV" ] || { echo "not executable: $DRV" >&2; exit 2; }
[ -r "$CADDYFILE" ] || { echo "no $CADDYFILE" >&2; exit 2; }
[ -d "$TR" ] || { echo "no tracefs at $TR" >&2; exit 2; }
docker image inspect "$IMG" >/dev/null 2>&1 || { echo "image $IMG missing" >&2; exit 2; }
docker inspect "$CNAME" >/dev/null 2>&1 && { echo "container $CNAME exists and is not this run's" >&2; exit 2; }
grep -q gcaddy "$TR/uprobe_events" && { echo "uprobe group gcaddy already registered" >&2; exit 2; }
for p in $CPORT $SPORT $PPORT $DPORT $DPUSH; do
	ss -ltn | grep -q ":$p " && { echo "port $p is in use" >&2; exit 2; }
done
mkdir -p "$R" "$W/caddy-data"

ROWS=$R/rows.tsv
: > "$ROWS"
row() {
	printf 'row\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" | tee -a "$ROWS"
}

SPID_C=
SPID_D=
RPID=
VPID=
WPID=
TPID=
TPID2=
TPID3=
OLDCLOCK=$(sed -n 's/.*\[\(.*\)\].*/\1/p' "$TR/trace_clock")
cleanup() {
	trap - EXIT INT TERM
	for p in $WPID $VPID $TPID $TPID2 $TPID3 $SPID_C $SPID_D; do
		[ -n "$p" ] && kill "$p" 2>/dev/null
	done
	docker rm -f "$CNAME" >/dev/null 2>&1
	for e in gc_sync gc_leave; do
		echo 0 > "$TR/events/gcaddy/$e/enable" 2>/dev/null
	done
	[ -n "$RPID" ] && kill "$RPID" 2>/dev/null
	for e in gc_sync gc_leave; do
		echo "-:gcaddy/$e" >> "$TR/uprobe_events" 2>/dev/null
	done
	echo "${OLDCLOCK:-local}" > "$TR/trace_clock" 2>/dev/null
}
trap cleanup EXIT INT TERM

fileoff() {
	addr=$($GO tool nm "$BIN" | awk -v s="$1" '$3==s{print $1}')
	[ -n "$addr" ] || { echo "no symbol $1" >&2; exit 2; }
	readelf -lW "$BIN" | python3 -I -c '
import re, sys
addr = int(sys.argv[1], 16)
for l in sys.stdin:
    f = l.split()
    if len(f) >= 8 and f[0] == "LOAD":
        off, va, fsz = int(f[1], 16), int(f[2], 16), int(f[4], 16)
        if "E" in f[6:] and va <= addr < va + fsz:
            print(hex(addr - va + off)); break
' "$addr"
}

SYNC_OFF=$(fileoff 'github.com/candyflipline/mnemonic/internal/intake.(*service).Sync')
LEAVE_OFF=$(fileoff 'github.com/candyflipline/mnemonic/internal/intake.(*service).leave')
[ -n "$SYNC_OFF" ] && [ -n "$LEAVE_OFF" ] || { echo "no file offsets" >&2; exit 2; }
echo mono > "$TR/trace_clock"
echo "p:gcaddy/gc_sync $BIN:$SYNC_OFF" >> "$TR/uprobe_events"
echo "p:gcaddy/gc_leave $BIN:$LEAVE_OFF" >> "$TR/uprobe_events"
echo 1 > "$TR/events/gcaddy/gc_sync/enable"
echo 1 > "$TR/events/gcaddy/gc_leave/enable"
cat "$TR/trace_pipe" > "$R/trace.txt" &
RPID=$!

count() { grep -c " $1:" "$R/trace.txt"; }
leaves() { count gc_leave; }
syncs() { count gc_sync; }
balance() {
	i=0
	while [ "$(syncs)" -ne "$(leaves)" ] && [ "$i" -lt 300 ]; do sleep 0.1; i=$((i + 1)); done
	[ "$(syncs)" -eq "$(leaves)" ]
}

start_serve() { # name listen push_listen
	d=$R/$1
	mkdir -p "$d/data" "$d/cfg"
	chmod 700 "$d/cfg"
	"$BIN" token new --author gcaddy > "$d/cfg/token.out"
	head -1 "$d/cfg/token.out" > "$d/cfg/token"
	chmod 600 "$d/cfg/token"
	{
		echo "data_dir = \"$d/data\""
		echo "listen = \"$2\""
		echo "push_listen = \"$3\""
		tail -n +2 "$d/cfg/token.out"
	} > "$d/cfg/server.toml"
	GRPC_GO_LOG_SEVERITY_LEVEL=info GOMAXPROCS=4 "$BIN" serve --config "$d/cfg/server.toml" > "$R/$1-serve.log" 2>&1 &
	pid=$!
	i=0
	until grep -q 'mnemonic serve: HTTP .*, push ' "$R/$1-serve.log"; do
		kill -0 "$pid" 2>/dev/null || { echo "serve $1 died" >&2; cat "$R/$1-serve.log" >&2; exit 1; }
		i=$((i + 1)); [ "$i" -lt 100 ] || { echo "serve $1 not up" >&2; exit 1; }
		sleep 0.2
	done
	echo "$pid"
}
SPID_C=$(start_serve C "172.17.0.1:$SPORT" "172.17.0.1:$PPORT") || exit 1
SPID_D=$(start_serve D "127.0.0.1:$DPORT" "127.0.0.1:$DPUSH") || exit 1
TOKC=$(cat "$R/C/cfg/token")
TOKD=$(cat "$R/D/cfg/token")
URLC=https://localhost:$CPORT
URLD=http://127.0.0.1:$DPUSH

cp "$CADDYFILE" "$R/Caddyfile.neg1"
grep -v 'flush_interval' "$CADDYFILE" > "$R/Caddyfile.default"
grep -q flush_interval "$R/Caddyfile.default" && { echo "variant still has flush_interval" >&2; exit 2; }
grep -q 'flush_interval -1' "$R/Caddyfile.neg1" || { echo "committed Caddyfile has no flush_interval -1" >&2; exit 2; }
diff "$R/Caddyfile.neg1" "$R/Caddyfile.default" > "$R/Caddyfile.diff"

DOCKER_RUN="docker run -d --name $CNAME --add-host host.docker.internal:host-gateway -p 127.0.0.1:$CPORT:$CPORT -e MORAINE_HOST=localhost:$CPORT -v <Caddyfile>:/etc/caddy/Caddyfile:ro -v $W/caddy-data:/data $IMG"
printf 'docker_run\t%s\n' "$DOCKER_RUN"
caddy_up() { # setting
	docker rm -f "$CNAME" >/dev/null 2>&1
	docker run -d --name "$CNAME" --add-host host.docker.internal:host-gateway \
		-p 127.0.0.1:$CPORT:$CPORT -e MORAINE_HOST=localhost:$CPORT \
		-v "$R/Caddyfile.$1:/etc/caddy/Caddyfile:ro" -v "$W/caddy-data:/data" "$IMG" > /dev/null || return 1
	i=0
	until docker logs "$CNAME" 2>&1 | grep -q 'serving initial configuration'; do
		i=$((i + 1)); [ "$i" -lt 120 ] || { docker logs "$CNAME" >&2; return 1; }
		sleep 0.5
	done
	ROOT=$W/caddy-data/caddy/pki/authorities/local/root.crt
	i=0
	until [ -s "$ROOT" ]; do i=$((i + 1)); [ "$i" -lt 60 ] || return 1; sleep 0.5; done
	cat /etc/ssl/certs/ca-certificates.crt "$ROOT" > "$R/bundle.pem"
	i=0
	until SSL_CERT_FILE=$R/bundle.pem "$DRV" lat -server "$URLC" -token "$TOKC" -machine "warm-$1-$i" -n 1 >/dev/null 2>&1; do
		i=$((i + 1)); [ "$i" -lt 60 ] || return 1
		sleep 0.5
	done
	return 0
}
caddy_down() { # tag
	docker logs "$CNAME" > "$R/caddy-$1.log" 2>&1
	l0=$(leaves)
	docker rm -f "$CNAME" >/dev/null 2>&1
	sleep 1
	balance
	echo $(( $(leaves) - l0 ))
}

SSL=$R/bundle.pem
cfg_client() { # name server tokenfile machine
	cat > "$R/client-$1.toml" <<EOF
server = "$2"
token_file = "$3"
machine = "$4"
EOF
}
frames() { # data_dir machine
	( cd "$1/raw" && find . -type f -name '*.zst' -path "*machine=$2/*" -print0 | xargs -0 -r sha256sum | sed 's#  \./#  #' | LC_ALL=C sort -k2 )
}
sources() { # data_dir machine  — manifests minus updated_ms
	( cd "$1/raw" && find . -type f -name '*.json' -path "*machine=$2/*" | LC_ALL=C sort | while read -r f; do printf '%s ' "$f"; jq -cS 'del(.updated_ms)' "$f"; done )
}

# ---- phase A: the stream through Caddy as committed
caddy_up neg1 || { echo "caddy did not come up" >&2; exit 1; }
SETA=neg1

mkdir -p "$R/home-conv" "$R/home-32" "$R/home-idle"
python3 -I - "$R" <<'PY'
import os, sys
r = sys.argv[1]
def w(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f: f.write(data)
def lines(n, size):
    return b"".join(b'{"t":"conv","i":%d,"x":"%s"}\n' % (i, (b"lorem ipsum dolor sit amet " * (size // 27 + 1))[:size]) for i in range(n))
h = r + "/home-conv"
w(h + "/.claude/projects/pa/s1.jsonl", lines(1, 100))
w(h + "/.claude/projects/pa/s2.jsonl", lines(100, 200))
w(h + "/.claude/projects/pb/s3.jsonl", lines(1, 1 << 20))
w(h + "/.claude/projects/pb/s4.jsonl", lines(50, 100 << 10))
w(h + "/.claude/projects/pc/s5.jsonl", lines(200, 100 << 10))
w(h + "/.codex/sessions/2026/10/25/x.jsonl", lines(30, 300))
total = (32 << 20) - 5
pre, post = b'{"t":"big","x":"', b'"}\n'
filler = (b"lorem ipsum dolor sit amet " * (total // 27 + 1))[: total - len(pre) - len(post)]
w(r + "/home-32/.claude/projects/p32/big.jsonl", pre + filler + post + b'{"t":"tail"}\n')
w(r + "/home-idle/.claude/projects/pi/idle.jsonl", lines(3, 80))
PY

cfg_client c-conv "$URLC" "$R/C/cfg/token" conv
cfg_client d-conv "$URLD" "$R/D/cfg/token" conv
cfg_client c-32 "$URLC" "$R/C/cfg/token" m32
cfg_client d-32 "$URLD" "$R/D/cfg/token" m32
cfg_client c-idle "$URLC" "$R/C/cfg/token" idle

# converge: the same three-push sequence through Caddy and direct, then compare stores
cpush() { HOME=$R/home-conv SSL_CERT_FILE=$SSL "$BIN" push --config "$R/client-$1-conv.toml" 2>&1; }
p1=$(cpush c); rc1=$?
p2=$(cpush c); rc2=$?
d1=$(cpush d); drc1=$?
d2=$(cpush d); drc2=$?
printf '{"t":"more"}\n' >> "$R/home-conv/.claude/projects/pa/s2.jsonl"
p3=$(cpush c); rc3=$?
d3=$(cpush d); drc3=$?
frames "$R/C/data" conv > "$R/conv-c.frames"; frames "$R/D/data" conv > "$R/conv-d.frames"
sources "$R/C/data" conv > "$R/conv-c.src"; sources "$R/D/data" conv > "$R/conv-d.src"
nfr=$(wc -l < "$R/conv-c.frames")
if [ "$rc1$rc2$rc3$drc1$drc2$drc3" = 000000 ] && cmp -s "$R/conv-c.frames" "$R/conv-d.frames" && cmp -s "$R/conv-c.src" "$R/conv-d.src" && [ "$nfr" -eq 6 ] \
	&& echo "$p2" | grep -q 'appended 0, replaced 0, unchanged 6, failed 0' && echo "$p3" | grep -q 'appended 1, replaced 0, unchanged 5, failed 0'; then v=PASS; else v=FAIL; fi
row converge $SETA "rc=$rc1/$rc2/$rc3 frames=$nfr equal_direct_frames=$(cmp -s "$R/conv-c.frames" "$R/conv-d.frames" && echo yes || echo no) equal_direct_manifests=$(cmp -s "$R/conv-c.src" "$R/conv-d.src" && echo yes || echo no) push1=[$p1] push2=[$p2] push3=[$p3]" $v

# append32
t0=$(date +%s.%N)
o1=$(HOME=$R/home-32 SSL_CERT_FILE=$SSL "$BIN" push --config "$R/client-c-32.toml" 2>&1); rc1=$?
t1=$(date +%s.%N)
o2=$(HOME=$R/home-32 "$BIN" push --config "$R/client-d-32.toml" 2>&1); rc2=$?
frames "$R/C/data" m32 > "$R/m32-c.frames"; frames "$R/D/data" m32 > "$R/m32-d.frames"
sources "$R/C/data" m32 > "$R/m32-c.src"; sources "$R/D/data" m32 > "$R/m32-d.src"
sz=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b-a}')
if [ $rc1 -eq 0 ] && [ $rc2 -eq 0 ] && [ -s "$R/m32-c.frames" ] && cmp -s "$R/m32-c.frames" "$R/m32-d.frames" && cmp -s "$R/m32-c.src" "$R/m32-d.src" \
	&& echo "$o1" | grep -q 'failed 0'; then v=PASS; else v=FAIL; fi
row append32 $SETA "rc_caddy=$rc1 rc_direct=$rc2 wall_s=$sz client=[$o1] frame_sha=$(cut -c1-16 "$R/m32-c.frames") direct_sha=$(cut -c1-16 "$R/m32-d.frames") manifest=$(sed 's/^[^ ]* //' "$R/m32-c.src" | cut -c1-200)" $v

# stream300
vmstat 1 > "$R/vmstat-big.txt" &
VPID=$!
bigout=$(SSL_CERT_FILE=$SSL "$DRV" big -server "$URLC" -token "$TOKC" -machine big300 -mib 330 -chunk-mib 30 2>&1); rc=$?
kill "$VPID" 2>/dev/null; VPID=
bsize=$(sed -n 's/.*"size":\([0-9]*\).*/\1/p' "$(find "$R/C/data/raw" -path '*machine=big300/*' -name '*.json' | head -1)")
cpu=$(awk 'NR>2{us+=$13; sy+=$14; wa+=$16; n++} END{if(n) printf "us=%.0f sy=%.0f wa=%.0f idle_samples=%d", us/n, sy/n, wa/n, n}' "$R/vmstat-big.txt")
if [ $rc -eq 0 ] && echo "$bigout" | grep -q '^sent=11 acked=11 applied=11 '; then v=PASS; else v=FAIL; fi
row stream300 $SETA "rc=$rc $bigout store_size_bytes=${bsize:-none} want=$((330 << 20)) box_cpu_pct_mean[$cpu]" $v
balance

# separate: what 8741 accepts is only what reaches it
sepok=1
l8741=$(ss -ltnH | awk '{print $4}' | grep ':8741$' | tr '\n' ',')
[ "$l8741" = "172.17.0.1:8741," ] || sepok=0
loop=$(python3 -I -c 'import socket
s = socket.socket(); s.settimeout(3)
try:
    s.connect(("127.0.0.1", 8741)); print("accepted")
except OSError as e:
    print(type(e).__name__)')
[ "$loop" = ConnectionRefusedError ] || sepok=0
http_push=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/grpc' -H "authorization: Bearer $TOKC" http://172.17.0.1:$SPORT/mnemonic.push.v1.Push/Sync)
[ "$http_push" = 404 ] || sepok=0
badtok=$(SSL_CERT_FILE=$SSL "$DRV" lat -server "$URLC" -token "x$TOKC" -machine badtok -n 1 2>&1 | head -1 | cut -c1-160)
case "$badtok" in *Unauthenticated*) ;; *) sepok=0;; esac
if [ $sepok -eq 1 ]; then v=PASS; else v=FAIL; fi
row separate $SETA "listeners_on_8741=[$l8741] loopback_127.0.0.1:8741=$loop http_listener_push_path_status=$http_push bad_token_via_caddy=[$badtok]" $v
balance

# idle: a fresh Caddy (and so a fresh upstream connection) is created while the captures run
caddy_down phaseA >/dev/null
cat > "$R/h2scan.py" <<'PY'
import bisect, struct, sys
# h2scan.py PCAP PORT MODE. MODE h2: plaintext HTTP/2 frames of the flows to/from PORT.
# MODE tls: TLS application-data records of length 34 (one PING frame) to/from PORT.
pcap, port, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3]
d = open(pcap, "rb").read()
magic = d[:4]
ns = magic == bytes.fromhex("4d3cb2a1")
le = magic in (bytes.fromhex("d4c3b2a1"), bytes.fromhex("4d3cb2a1"))
e = "<" if le else ">"
link = struct.unpack(e + "I", d[20:24])[0]
hl = {1: 14, 113: 16, 276: 20}[link]
flows = {}
t0 = None
off = 24
while off + 16 <= len(d):
    sec, frac, cap, _ = struct.unpack(e + "IIII", d[off:off + 16])
    pkt = d[off + 16:off + 16 + cap]
    off += 16 + cap
    ts = sec + frac / (1e9 if ns else 1e6)
    if t0 is None:
        t0 = ts
    ip = pkt[hl:]
    if len(ip) < 20 or ip[0] >> 4 != 4 or ip[9] != 6:
        continue
    ihl = (ip[0] & 15) * 4
    tl = struct.unpack(">H", ip[2:4])[0]
    tcp = ip[ihl:tl]
    sp, dp, seq = struct.unpack(">HHI", tcp[:8])
    fl = tcp[13]
    pl = tcp[(tcp[12] >> 4) * 4:]
    if port not in (sp, dp):
        continue
    cport = sp if dp == port else dp
    direction = "c2s" if dp == port else "s2c"
    f = flows.setdefault((cport, direction), {"isn": None, "segs": {}})
    if fl & 2:
        f["isn"] = seq
    if pl:
        f["segs"].setdefault(seq, (ts, pl))
def assemble(f):
    base = f["isn"] + 1 if f["isn"] is not None else min(f["segs"])
    out, marks, nxt = bytearray(), [], base
    for seq in sorted(f["segs"]):
        ts, pl = f["segs"][seq]
        if seq + len(pl) <= nxt:
            continue
        if seq > nxt:
            break
        pl = pl[nxt - seq:]
        marks.append((len(out), ts))
        out += pl
        nxt += len(pl)
    return bytes(out), marks
def when(marks, o):
    return marks[bisect.bisect_right([m[0] for m in marks], o) - 1][1] - t0
res = {}
for (cp, direction), f in sorted(flows.items()):
    buf, marks = assemble(f)
    if not buf:
        continue
    r = res.setdefault(direction, {"conns": 0, "ping": [], "pingack": [], "goaway": [], "rst": []})
    r["conns"] += 1
    if mode == "h2":
        p = 24 if buf.startswith(b"PRI * HTTP/2.0") else 0
        while p + 9 <= len(buf):
            ln = int.from_bytes(buf[p:p + 3], "big")
            ty = buf[p + 3]
            fl = buf[p + 4]
            t = when(marks, p)
            if ty == 6:
                r["pingack" if fl & 1 else "ping"].append(t)
            elif ty == 7 and p + 17 <= len(buf):
                r["goaway"].append((round(t, 1), int.from_bytes(buf[p + 13:p + 17], "big")))
            elif ty == 3 and p + 13 <= len(buf):
                r["rst"].append((round(t, 1), int.from_bytes(buf[p + 9:p + 13], "big")))
            p += 9 + ln
    else:
        p = 0
        while p + 5 <= len(buf):
            ty = buf[p]
            ln = int.from_bytes(buf[p + 3:p + 5], "big")
            if ty == 23 and ln == 34:
                r["ping"].append(when(marks, p))
            p += 5 + ln
def gaps(xs):
    xs = sorted(xs)
    g = [b - a for a, b in zip(xs, xs[1:])]
    return "gap_s min=%.1f max=%.1f" % (min(g), max(g)) if g else "gap_s none"
for direction in ("c2s", "s2c"):
    r = res.get(direction)
    if not r:
        print("%s conns=0 ping=0 pingack=0 goaway=0 ping_records=0" % direction)
        continue
    if mode == "h2":
        print("%s conns=%d ping=%d pingack=%d goaway=%d%s rst=%d%s %s" % (direction, r["conns"], len(r["ping"]), len(r["pingack"]), len(r["goaway"]), r["goaway"], len(r["rst"]), r["rst"], gaps(r["ping"])))
    else:
        print("%s conns=%d ping_records=%d %s" % (direction, r["conns"], len(r["ping"]), gaps(r["ping"])))
PY
cap_up() { # tag filter iface
	tcpdump -U -n -i "$3" -w "$R/$1.pcap" "$2" > "$R/$1.tcpdump.log" 2>&1 &
	eval "TP_$1=$!"
	i=0
	until grep -q 'listening on' "$R/$1.tcpdump.log"; do
		i=$((i + 1)); [ "$i" -lt 50 ] || { cat "$R/$1.tcpdump.log" >&2; return 1; }
		sleep 0.1
	done
}
cap_up h2c "tcp port $PPORT" docker0 || exit 1
TPID=$TP_h2c
cap_up tls "tcp port $CPORT" docker0 || exit 1
TPID2=$TP_tls
cap_up tlslo "tcp port $CPORT" lo || exit 1
TPID3=$TP_tlslo
caddy_up neg1 || { echo "caddy did not come up (idle)" >&2; exit 1; }
balance
s0=$(syncs); l0=$(leaves)
HOME=$R/home-idle SSL_CERT_FILE=$SSL "$BIN" push --watch --config "$R/client-c-idle.toml" > "$R/idle.out" 2>&1 &
WPID=$!
i=0
until [ "$(wc -l < "$R/idle.out")" -ge 1 ] || [ $i -ge 300 ]; do i=$((i + 1)); sleep 0.1; done
n1=$(wc -l < "$R/idle.out")
sleep "$IDLE_S"
s1=$(syncs); l1=$(leaves)
tA=$(date +%s.%N)
printf '{"t":"idle-append"}\n' >> "$R/home-idle/.claude/projects/pi/idle.jsonl"
i=0
until [ "$(wc -l < "$R/idle.out")" -gt "$n1" ] || [ $i -ge 300 ]; do i=$((i + 1)); sleep 0.05; done
tB=$(date +%s.%N)
lag=$(awk -v a="$tA" -v b="$tB" 'BEGIN{printf "%.2f", b-a}')
last=$(tail -1 "$R/idle.out")
sleep 2
kill "$TPID" "$TPID2" "$TPID3" 2>/dev/null
wait "$TPID" "$TPID2" "$TPID3" 2>/dev/null
TPID=; TPID2=; TPID3=
kill "$WPID" 2>/dev/null; WPID=
caddy_down phaseA2 >/dev/null
python3 -I "$R/h2scan.py" "$R/h2c.pcap" $PPORT h2 > "$R/ping-caddy-to-8741.txt" 2>&1
python3 -I "$R/h2scan.py" "$R/tls.pcap" $CPORT tls > "$R/ping-client-to-caddy.txt" 2>&1
python3 -I "$R/h2scan.py" "$R/tlslo.pcap" $CPORT tls > "$R/ping-client-to-caddy-lo.txt" 2>&1
for f in ping-caddy-to-8741 ping-client-to-caddy ping-client-to-caddy-lo; do sed "s/^/ping_evidence\t$f\t/" "$R/$f.txt"; done
n_srv=$(( IDLE_S / 45 - 1 )); n_cli=$(( IDLE_S / 30 - 1 ))
getn() { sed -n "s/^$2 .*[ ]$3=\([0-9]*\).*/\1/p" "$1" | head -1; }
srv_ping=$(getn "$R/ping-caddy-to-8741.txt" s2c ping)
srv_ack=$(getn "$R/ping-caddy-to-8741.txt" c2s pingack)
cad_ping=$(getn "$R/ping-caddy-to-8741.txt" c2s ping)
go_c=$(getn "$R/ping-caddy-to-8741.txt" c2s goaway); go_s=$(getn "$R/ping-caddy-to-8741.txt" s2c goaway)
cli_ping=$(getn "$R/ping-client-to-caddy.txt" c2s ping_records)
cli_ack=$(getn "$R/ping-client-to-caddy.txt" s2c ping_records)
cli_ping_lo=$(getn "$R/ping-client-to-caddy-lo.txt" c2s ping_records)
cli_ack_lo=$(getn "$R/ping-client-to-caddy-lo.txt" s2c ping_records)
[ "${cli_ping:-0}" -ge "${cli_ping_lo:-0}" ] || { cli_ping=$cli_ping_lo; cli_ack=$cli_ack_lo; }
bad_logs=$(cat "$R/C-serve.log" "$R/caddy-phaseA2.log" "$R/idle.out" | grep -Eci 'GOAWAY|ENHANCE_YOUR_CALM|too_many_pings')
if [ $((s1 - s0)) -eq 1 ] && [ $((l1 - l0)) -eq 0 ] && [ "$(wc -l < "$R/idle.out")" -eq $((n1 + 1)) ] && echo "$last" | grep -q 'appended 1' \
	&& awk -v x="$lag" 'BEGIN{exit !(x <= 5)}' && [ "$IDLE_S" -ge 660 ] \
	&& [ "${srv_ping:-0}" -ge "$n_srv" ] && [ "${srv_ack:-0}" -ge "$n_srv" ] && [ "${go_c:-1}" -eq 0 ] && [ "${go_s:-1}" -eq 0 ] \
	&& [ "${cli_ping:-0}" -ge "$n_cli" ] && [ "${cli_ack:-0}" -ge "$n_cli" ] && [ "$bad_logs" -eq 0 ]; then v=PASS; else v=FAIL; fi
row idle $SETA "idle_s=$IDLE_S ack_after_append_s=$lag streams_opened=$((s1 - s0)) streams_ended=$((l1 - l0)) client_lines_before=$n1 client_lines_after=$(wc -l < "$R/idle.out") last=[$last] caddy_to_8741(serve_pings=${srv_ping:-?} caddy_pings=${cad_ping:-?} caddy_pingacks=${srv_ack:-?} goaway_c2s=${go_c:-?} goaway_s2c=${go_s:-?} want_serve_pings>=$n_srv) client_to_caddy(client_ping_records=${cli_ping:-?} caddy_reply_records=${cli_ack:-?} want>=$n_cli) goaway_or_ratelimit_lines_in_logs=$bad_logs" $v

# ---- phase B: alternating arms, latency and cancellation
: > "$R/kills.tsv"
arm_url() { case $1 in direct) echo "$URLD";; *) echo "$URLC";; esac; }
arm_tok() { case $1 in direct) echo "$TOKD";; *) echo "$TOKC";; esac; }
session() { # arm round
	arm=$1; r=$2; url=$(arm_url "$arm"); tok=$(arm_tok "$arm")
	if [ "$arm" != direct ]; then caddy_up "$arm" || { echo "caddy up failed ($arm $r)" >&2; return 1; }; fi
	balance
	SSL_CERT_FILE=$SSL "$DRV" lat -server "$url" -token "$tok" -machine "lat-$arm-$r" -n "$LATN" > "$R/lat-$arm-$r.txt" 2>&1
	balance
	SSL_CERT_FILE=$SSL "$DRV" hold -server "$url" -token "$tok" -machine "kill-$arm-$r" > "$R/hold-$arm-$r.out" 2>&1 &
	hp=$!
	i=0
	until grep -q ready "$R/hold-$arm-$r.out" || [ $i -ge 200 ]; do i=$((i + 1)); sleep 0.1; done
	sleep 0.3
	c0=$(leaves)
	t0=$(python3 -I -c 'import os,sys,time;t=time.clock_gettime(time.CLOCK_MONOTONIC);os.kill(int(sys.argv[1]),9);print("%.6f"%t)' "$hp")
	wait "$hp" 2>/dev/null
	j=0
	while [ "$(leaves)" -le "$c0" ] && [ $j -lt $((KILLCAP * 20)) ]; do sleep 0.05; j=$((j + 1)); done
	printf '%s\t%s\t%s\t%s\n' "$arm" "$r" "$t0" "$c0" >> "$R/kills.tsv"
	if [ "$arm" != direct ]; then
		orph=$(caddy_down "$arm-$r")
		printf '%s\t%s\t%s\n' "$arm" "$r" "$orph" >> "$R/orphans.tsv"
	fi
	balance
}
: > "$R/orphans.tsv"
r=1
while [ $r -le "$ROUNDS" ]; do
	if [ $((r % 2)) -eq 1 ]; then ORDER="neg1 default direct"; else ORDER="direct default neg1"; fi
	for arm in $ORDER; do
		session "$arm" "$r" || exit 1
	done
	r=$((r + 1))
done

python3 -I - "$R" "$ROUNDS" "$KILLCAP" <<'PY' | while IFS=$(printf '\t') read -r a b c d; do row "$a" "$b" "$c" "$d"; done
import re, statistics, sys, os
r, rounds, cap = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
leaves = []
for l in open(r + "/trace.txt"):
    m = re.search(r"\s(\d+\.\d+): gc_leave:", l)
    if m: leaves.append(float(m.group(1)))
leaves.sort()
def q(xs, p):
    xs = sorted(xs); k = (len(xs) - 1) * p; f = int(k); c = min(f + 1, len(xs) - 1)
    return xs[f] + (xs[c] - xs[f]) * (k - f)
kills = {}
for l in open(r + "/kills.tsv"):
    arm, rd, t0, c0 = l.split()
    t0 = float(t0)
    nxt = [x for x in leaves if x > t0]
    kills.setdefault(arm, []).append((nxt[0] - t0) if nxt and nxt[0] - t0 <= cap else None)
orph = {}
for l in open(r + "/orphans.tsv"):
    arm, rd, n = l.split(); orph.setdefault(arm, []).append(int(n))
for arm in ("neg1", "default", "direct"):
    xs = []; errs = 0; runs = 0
    for rd in range(1, rounds + 1):
        p = "%s/lat-%s-%d.txt" % (r, arm, rd)
        if not os.path.exists(p): continue
        runs += 1
        for l in open(p):
            if l.startswith("lat_us "): xs.append(int(l.split()[1]) / 1000.0)
            elif l.startswith("error") or (l.startswith("done") and l.strip() != "done errors=0"): errs += 1
    if xs:
        ok = errs == 0 and max(xs) < 2000
        print("latency\t%s\tn=%d runs=%d errors=%d median_ms=%.2f p95_ms=%.2f min_ms=%.2f max_ms=%.2f\t%s" % (arm, len(xs), runs, errs, statistics.median(xs), q(xs, .95), min(xs), max(xs), "PASS" if ok else "FAIL"))
    else:
        print("latency\t%s\tn=0 errors=%d\tFAIL" % (arm, errs))
for arm in ("neg1", "default", "direct"):
    ks = kills.get(arm, [])
    done = [k for k in ks if k is not None]
    if done:
        s = "kills=%d ended=%d median_ms=%.1f min_ms=%.1f max_ms=%.1f" % (len(ks), len(done), statistics.median(done) * 1000, min(done) * 1000, max(done) * 1000)
    else:
        s = "kills=%d ended=0" % len(ks)
    if arm != "direct":
        s += " orphans_ended_only_at_container_stop=%s" % ",".join(map(str, orph.get(arm, [])))
    print("cancel\t%s\t%s\t%s" % (arm, s, "PASS" if len(done) == len(ks) and ks else "FAIL"))
PY

printf 'run_dir\t%s\n' "$R"
exit 0
