#!/bin/sh
# archive-parity.sh — lane U9e: archive parity, engine vs the 59796bc oracle,
# over a store holding the frozen raw corpus plus the 16 ClickHouse archive
# copies (ch-*), on the real corpus.
#
## Phases (one per invocation; the CALLER holds the box lock around it):
##   setup   arch-oracle (cp -a of cold-1, then the 16 ch-* files hardlinked
##           into its events/ tree at their exact relative paths), arch-root
##           (an engine dataroot: raw/raw symlinked to the frozen raw, the 16
##           ch-* files hardlinked under events/), arena-arch (golden/hidden/
##           check/runs skeleton; masks copied from the raw-only arena).
##   oracle  one oracle ingest -root $BOX -store arch-oracle (timed, time -v,
##           vmstat alongside, caches dropped) + archive-session counts from
##           the oracle side (list_sessions, cursor-paginated).
##   gen     gen seed a/b from arch-oracle into arena-arch golden/hidden; how
##           many calls touch an archive-only session; more seeds while the
##           combined count is under MIN_ARCH (default 10), each swap logged.
##   check   check -bin ENG/wrapper-engine -arena arena-arch -name
##           engine-arch-<short> -skip incremental,crash,appendcrash.
##   build   build the engine from a git-archive tarball (build-engine.sh's
##           layout; -tags server when that links under CGO_ENABLED=0, else
##           no tags — recorded in sha256.txt).
##   deploy  3 timed engine cold ingests of raw+archive (fresh store each,
##           dropped caches, GOMAXPROCS=16, vmstat/pidstat alongside) + du
##           and the archive member count.
##
## Env: BOX (default /var/lib/mnemonic-e3), OUT (default $BOX/out/u9e),
##      BIN (oracle tree with the copyRaw-fixed oracle + gen/check/seed),
##      ENG (engine bin dir from `build`), MIN_ARCH, SEEDS, RUNS, FIRST.
## Counts and timings only; never prints corpus content.
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
OUT=${OUT:-$BOX/out/u9e}
BIN=${BIN:-$(ls -d "$BOX"/bin-333fbd6* 2>/dev/null | head -1)}
ENG=${ENG:-$(ls -d "$BOX"/bin-dcfe552* 2>/dev/null | head -1)}
MIN_ARCH=${MIN_ARCH:-10}
SEEDS=${SEEDS:-"3 4"}
RUNS=${RUNS:-3}
FIRST=${FIRST:-1}
CHROOT=$BOX/archive
ARCH=$BOX/arch-oracle
AROOT=$BOX/arch-root
ARENA=$BOX/arena-arch

mkdir -p "$OUT"
log() { echo "[$(date -u +%H:%M:%SZ)] $*" >> "$OUT/parity.log"; }
fail() { log "FAIL: $*"; echo "FAIL: $*" >&2; exit 1; }

phase_setup() {
	log "setup: begin"
	find "$CHROOT/events" -name 'ch-*.parquet' | sort > "$OUT/ch-list.txt"
	N=$(wc -l < "$OUT/ch-list.txt")
	[ "$N" -eq 16 ] || fail "expected 16 ch-* files under $CHROOT/events, found $N"
	df --output=avail -B1 / | tail -1 > "$OUT/setup-df-before.txt"

	rm -rf "$ARCH"
	mkdir -p "$ARCH"
	du -s --block-size=1 "$BOX/cold-1" > "$OUT/cold-1-bytes.txt"
	log "setup: cp -a cold-1 ($(cat "$OUT/cold-1-bytes.txt") bytes) -> arch-oracle"
	cp -a "$BOX/cold-1/." "$ARCH/"
	log "setup: cp -a done"

	rm -rf "$AROOT"
	mkdir -p "$AROOT/raw"
	while IFS= read -r p; do
		rel=${p#"$CHROOT/"}
		mkdir -p "$ARCH/$(dirname "$rel")" "$AROOT/$(dirname "$rel")"
		ln "$p" "$ARCH/$rel"
		ln "$p" "$AROOT/$rel"
	done < "$OUT/ch-list.txt"
	ln -sfn "$BOX/raw" "$AROOT/raw/raw"

	mkdir -p "$ARENA/golden" "$ARENA/hidden" "$ARENA/check" "$ARENA/runs"
	cp -f "$BOX/arena/check/masks.json" "$ARENA/check/masks.json"
	ln -sfn "$AROOT" "$ARENA/raw"

	A=$(find "$ARCH/events" -name 'ch-*.parquet' | wc -l)
	R=$(find "$AROOT/events" -name 'ch-*.parquet' | wc -l)
	[ "$A" -eq 16 ] && [ "$R" -eq 16 ] || fail "hardlink count arch=$A root=$R, want 16/16"
	[ -e "$AROOT/raw/raw" ] || fail "arch-root/raw/raw missing"
	[ -s "$ARENA/check/masks.json" ] || fail "masks copy missing"
	du -s --block-size=1 "$ARCH" > "$OUT/arch-oracle-bytes.txt"
	df --output=avail -B1 / | tail -1 > "$OUT/setup-df-after.txt"
	log "setup: done (ch hardlinks arch=$A root=$R; disk avail before $(cat "$OUT/setup-df-before.txt") after $(cat "$OUT/setup-df-after.txt"))"
}

ls_reply() {
	# $1 store, $2 outprefix — cursor-paginated list_sessions into $2.jsonl
	: > "$2.jsonl"
	CURSOR_FILE=$2.cursor
	: > "$CURSOR_FILE"
	i=0
	while [ "$i" -lt 50 ]; do
		i=$((i + 1))
		CURSOR=$(cat "$CURSOR_FILE")
		python3 -I - "$2" "$CURSOR" <<'PYEOF'
import json, sys
path, cursor = sys.argv[1], sys.argv[2]
args = {"limit": 100000}
if cursor:
    args["cursor"] = cursor
with open(path + ".call.jsonl", "w") as f:
    f.write(json.dumps({"id": 9001, "tool": "list_sessions", "args": args}) + "\n")
PYEOF
		"$BIN/oracle" answer -root "$1" < "$2.call.jsonl" >> "$2.jsonl" 2>> "$2.err"
		python3 -I - "$2" <<'PYEOF'
import json, sys
path = sys.argv[1]
cursor = ""
with open(path + ".jsonl") as f:
    for line in f:
        if line.strip():
            r = json.loads(line)
            nc = (r.get("data") or {}).get("next_cursor") if isinstance(r.get("data"), dict) else None
            if nc:
                cursor = nc
with open(path + ".cursor", "w") as f:
    f.write(cursor)
PYEOF
		[ -s "$CURSOR_FILE" ] || break
	done
	rm -f "$CURSOR_FILE" "$2.call.jsonl"
}

phase_oracle() {
	log "oracle: begin (converge of the archive into arch-oracle)"
	sync
	echo 3 > /proc/sys/vm/drop_caches
	LOG=$OUT/oracle-arch-ingest.log
	: > "$LOG"
	vmstat 1 >> "$LOG" 2>&1 &
	VMS=$!
	START=$(date +%s)
	set +e
	/usr/bin/time.gnu -v "$BIN/wrapper-oracle" ingest -root "$BOX" -store "$ARCH" >> "$LOG" 2>&1
	RC=$?
	set -e
	[ -n "${VMS:-}" ] && kill "$VMS" 2>/dev/null || true
	WALL=$(( $(date +%s) - START ))
	log "oracle: ingest exit=$RC wall=${WALL}s"
	[ "$RC" -eq 0 ] || fail "oracle ingest exited $RC (see oracle-arch-ingest.log)"
	grep -E "Maximum resident set size|Elapsed \(wall" "$LOG" | tail -2 >> "$OUT/parity.log" || true
	grep -o "ingest: sources=.*" "$LOG" | tail -1 >> "$OUT/parity.log" || true

	ls_reply "$ARCH" "$OUT/ls-arch"
	ls_reply "$BOX/cold-1" "$OUT/ls-raw"
	python3 -I - "$OUT" <<'PYEOF' >> "$OUT/parity.log"
import json, os, sys
out = sys.argv[1]
def sessions(path):
    ids = set()
    with open(path) as f:
        for line in f:
            if not line.strip():
                continue
            r = json.loads(line)
            data = r.get("data")
            if not isinstance(data, dict):
                continue
            for s in data.get("sessions") or []:
                sid = s.get("id") or s.get("session_id")
                if sid:
                    ids.add(sid)
    return ids
arch = sessions(os.path.join(out, "ls-arch.jsonl"))
raw = sessions(os.path.join(out, "ls-raw.jsonl"))
only = arch - raw
with open(os.path.join(out, "arch-only-ids.json"), "w") as f:
    json.dump(sorted(only), f)
print("oracle: arch-oracle sessions %d, cold-1 %d, archive-only %d" % (len(arch), len(raw), len(only)))
PYEOF
	log "oracle: counts in oracle-counts line above"
}

arch_touch() {
	# $1 = dir with calls.jsonl+expected.jsonl -> prints archive-touching count
	python3 -I - "$OUT" "$1" <<'PYEOF'
import json, os, re, sys
out, d = sys.argv[1], sys.argv[2]
only = set(json.load(open(os.path.join(out, "arch-only-ids.json"))))
uuid = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")
exp = {}
ep = os.path.join(d, "expected.jsonl")
if os.path.exists(ep):
    for ln in open(ep):
        if ln.strip():
            r = json.loads(ln)
            exp[r.get("id")] = ln
hits = 0
for line in open(os.path.join(d, "calls.jsonl")):
    if not line.strip():
        continue
    c = json.loads(line)
    blob = line + exp.get(c.get("id"), "")
    us = set(u.lower() for u in uuid.findall(blob))
    if us & only:
        hits += 1
print(hits)
PYEOF
}

phase_gen() {
	log "gen: begin"
	set -- $SEEDS
	SG=$1
	SH=$2
	"$BIN/gen" -root "$ARCH" -oracle "$BIN/oracle" -seed "$SG" -out "$ARENA/golden" > "$OUT/gen-golden.log" 2>&1
	"$BIN/gen" -root "$ARCH" -oracle "$BIN/oracle" -seed "$SH" -out "$ARENA/hidden" > "$OUT/gen-hidden.log" 2>&1
	G=$(arch_touch "$ARENA/golden")
	H=$(arch_touch "$ARENA/hidden")
	log "gen: archive-touching calls golden=$G hidden=$H (seeds $SG/$SH)"
	seed=$SH
	while [ $((G + H)) -lt "$MIN_ARCH" ] && [ "$seed" -lt 9 ]; do
		seed=$((seed + 1))
		X=$OUT/gen-extra-$seed
		rm -rf "$X"
		mkdir -p "$X"
		"$BIN/gen" -root "$ARCH" -oracle "$BIN/oracle" -seed "$seed" -out "$X" > "$OUT/gen-extra-$seed.log" 2>&1
		C=$(arch_touch "$X")
		log "gen: extra seed $seed archive-touching=$C (golden=$G hidden=$H)"
		if [ "$C" -gt "$G" ] || [ "$C" -gt "$H" ]; then
			if [ "$G" -le "$H" ]; then T=golden; G=$C; else T=hidden; H=$C; fi
			rm -rf "$ARENA/$T"
			mv "$X" "$ARENA/$T"
			log "gen: replaced $T with seed $seed"
		fi
	done
	log "gen: final archive-touching golden=$G hidden=$H total=$((G + H))"
}

phase_check() {
	[ -n "$ENG" ] || fail "check needs ENG=<engine bin dir>"
	NAME=${NAME:-engine-arch-$(basename "$(dirname "$ENG")" | cut -d- -f2 | cut -c1-7)}
	log "check: begin name=$NAME"
	set +e
	"$BIN/check" -bin "$ENG/wrapper-engine" -name "$NAME" -arena "$ARENA" \
		-skip incremental,crash,appendcrash > "$OUT/engine-arch-check.log" 2>&1
	RC=$?
	set -e
	log "check: exit=$RC (scorecard is truth)"
	SC=$(ls -t "$ARENA"/runs/"$NAME"-*.json | head -1)
	python3 -I - "$SC" > "$OUT/engine-arch-summary.txt" <<'PYEOF'
import json, sys
c = json.load(open(sys.argv[1]))
print("pass", c.get("pass"))
for section in ("public", "hidden"):
    p = c.get(section) or {}
    print(section, "pass", p.get("pass"))
    for tool, t in sorted((p.get("tools") or {}).items()):
        print("  %s %d/%d" % (tool, t.get("pass", 0), t.get("pass", 0) + t.get("fail", 0)))
        for d in t.get("first_diffs") or []:
            print("   diff %s id=%s %s" % (d.get("set"), d.get("id"), ",".join(d.get("paths") or [])))
ing = c.get("clean_ingest") or {}
print("clean_ingest_wall_s %.1f peak_rss_mib %.1f" %
      ((ing.get("wall_ms") or 0) / 1000.0, (ing.get("peak_rss_bytes") or 0) / 1048576.0))
print("store_bytes", c.get("store_bytes"))
PYEOF
	log "check: summary written to engine-arch-summary.txt"
	cat "$OUT/engine-arch-summary.txt" >> "$OUT/parity.log"
}

phase_build() {
	TARBALL=${1:?usage: archive-parity.sh build TARBALL TREE}
	TREE=${2:?usage: archive-parity.sh build TARBALL TREE}
	SRC=/root/src/engine-build-u9e
	BINOUT=$BOX/bin-$TREE
	export PATH=/usr/local/go/bin:$PATH
	GOFLAGS=${GOFLAGS:--p=2}
	export GOFLAGS
	rm -rf "$SRC"
	mkdir -p "$SRC"
	tar -xf "$TARBALL" -C "$SRC"
	cd "$SRC"
	rm -rf "$BINOUT"
	mkdir -p "$BINOUT"
	TAGS=server
	if ! CGO_ENABLED=0 go build -tags server -o "$BINOUT/mnemonic" ./cmd/mnemonic 2> "$OUT/build-tags-server.err"; then
		TAGS=none
		CGO_ENABLED=0 go build -o "$BINOUT/mnemonic" ./cmd/mnemonic
	fi
	{
		echo "source: engine-build-u9e of $TREE"
		echo "built: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		go version
		echo "tags: $TAGS (CGO_ENABLED=0)"
		sha256sum "$BINOUT/mnemonic"
	} > "$BINOUT/sha256.txt"
	cat "$BINOUT/sha256.txt"
	{
		echo '#!'/bin/sh
		echo 'export GOMAXPROCS=16'
		echo "exec \"$BINOUT/mnemonic\" index \"\$@\""
	} > "$BINOUT/wrapper-engine"
	chmod +x "$BINOUT/wrapper-engine"
	log "build: $TREE installed in $BINOUT (tags=$TAGS)"
}

phase_deploy() {
	[ -n "$ENG" ] || fail "deploy needs ENG=<engine bin dir>"
	export GOMAXPROCS=16
	unset GOMEMLIMIT || true
	: > "$OUT/engine-arch-deploy.tsv"
	STORE=""
	i=$FIRST
	while [ "$i" -lt $((FIRST + RUNS)) ]; do
		STORE=$BOX/u9e-deploy-$i
		rm -rf "$STORE"
		mkdir -p "$STORE"
		sync
		echo 3 > /proc/sys/vm/drop_caches
		LOG=$OUT/engine-arch-deploy-$i.log
		: > "$LOG"
		PIDS=""
		if command -v pidstat >/dev/null 2>&1; then
			pidstat -u -d -h 1 >> "$LOG" 2>&1 &
			PIDS=$!
		fi
		vmstat 1 >> "$LOG" 2>&1 &
		VMS=$!
		set +e
		/usr/bin/time.gnu -v "$ENG/wrapper-engine" ingest -root "$AROOT" -store "$STORE" >> "$LOG" 2>&1
		RC=$?
		set -e
		kill "$VMS" 2>/dev/null || true
		[ -z "$PIDS" ] || kill "$PIDS" 2>/dev/null || true
		wait "$VMS" 2>/dev/null || true
		[ -z "$PIDS" ] || wait "$PIDS" 2>/dev/null || true
		[ "$RC" -eq 0 ] || fail "deploy run $i exited $RC (see $LOG)"
		python3 -I - "$i" "$LOG" >> "$OUT/engine-arch-deploy.tsv" <<'PYEOF'
import re, sys
run, path = sys.argv[1], sys.argv[2]
t = open(path, encoding="utf-8", errors="replace").read()
m = re.search(r"Elapsed \(wall clock\) time[^\n]*: (\S+)", t)
secs = 0.0
if m:
    for p in m.group(1).split(":"):
        secs = secs * 60 + float(p)
m = re.search(r"Maximum resident set size \(kbytes\): (\d+)", t)
rss = int(m.group(1)) / 1024 if m else 0.0
m = re.search(r"User time \(seconds\): ([\d.]+)", t)
user = float(m.group(1)) if m else 0.0
m = re.search(r"System time \(seconds\): ([\d.]+)", t)
sys_ = float(m.group(1)) if m else 0.0
m = re.search(r"Percent of CPU this job got: (\S+)%", t)
cpu = m.group(1) if m else ""
def count(pat):
    m = re.search(pat, t)
    return m.group(1) if m else ""
print("\t".join([run, "%.1f" % secs, "%.1f" % rss, "%.1f" % user, "%.1f" % sys_, cpu,
                 count(r"unchanged=(\d+)"), count(r"resumed=(\d+)"), count(r"fresh=(\d+)"), count(r"reparsed=(\d+)")]))
PYEOF
		log "deploy: run $i row: $(tail -1 "$OUT/engine-arch-deploy.tsv" | cut -f1-6)"
		i=$((i + 1))
	done
	python3 -I - "$OUT/engine-arch-deploy.tsv" >> "$OUT/parity.log" <<'PYEOF'
import sys
rows = [l.split("\t") for l in open(sys.argv[1]).read().splitlines() if l.strip()]
wall = sorted(float(r[1]) for r in rows)
rss = sorted(float(r[2]) for r in rows)
med = lambda xs: xs[len(xs) // 2] if len(xs) % 2 else (xs[len(xs) // 2 - 1] + xs[len(xs) // 2]) / 2
print("deploy: median_wall_s %.1f range %.1f-%.1f; median_rss_mib %.1f range %.1f-%.1f (runs %d)" %
      (med(wall), wall[0], wall[-1], med(rss), rss[0], rss[-1], len(rows)))
PYEOF
	M=$(ls "$STORE/src" 2>/dev/null | grep -c '~' || true)
	log "deploy: archive member ckpts in $STORE/src: $M"
	du -s --block-size=1 "$STORE" > "$OUT/deploy-store-bytes.txt"
	log "deploy: store bytes $(cat "$OUT/deploy-store-bytes.txt")"
	rm -rf "$STORE"
}

case "${1:?usage: archive-parity.sh setup|oracle|gen|check|build|deploy}" in
setup) phase_setup ;;
oracle) phase_oracle ;;
gen) phase_gen ;;
check) phase_check ;;
build) shift; phase_build "$@" ;;
deploy) phase_deploy ;;
*) fail "unknown phase $1" ;;
esac
