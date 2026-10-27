#!/bin/sh
set -eu
BOX=${BOX:-/var/lib/mnu}
OUT=${OUT:-$BOX/out}
SRC=${SRC:-$BOX/geq-src}
LOCK=$BOX/box.lock
H=$BOX/base-bin/mnemonic-83b0042
S=${S:-$BOX/final-bin/mnemonic-t5a03935}
SB=${SB:-$BOX/geq-bin/mnemonic-5a03935-bump}
DEN=${DEN:-32}
ROUNDS=${ROUNDS:-5}
STEPS=${STEPS:-prep matrix lagS lagH traceS sweep traceH fullH fullS}
LOG=$OUT/geq-driver.log
FH=$BOX/geqf-http-1
FS=$BOX/geqf-stream-1
CH=$BOX/geqc-http
CS=$BOX/geqc-stream
say() { echo "$(TZ=America/Los_Angeles date '+%H:%M:%S PT') $*" >> "$LOG"; }
L() { if [ -n "${INLOCK:-}" ]; then "$@"; else flock "$LOCK" "$@"; fi; }
snap() { find "$1" -type f -printf '%p %i %s %T@\n' | LC_ALL=C sort | sha256sum | cut -d' ' -f1; }
lag() {
	nm=$1; bin=$2; dir=$3; shift 3
	say "$nm wait"
	L env "$@" SERVE_WRAP="systemd-run --scope -q -p MemoryMax=18G -p MemorySwapMax=0" FIRST_MAX=900 IDX_MAX=900 sh "$SRC/lag.sh" "$bin" "$dir" > "$OUT/geq-$nm.txt" 2>&1 || say "$nm rc!=0"
	say "$nm done"
}

say "driver start steps=$STEPS inlock=${INLOCK:-0}"
until grep -q '^calls_passed' "$OUT/geq-parity.log" 2>/dev/null; do sleep 10; done
if ! grep -q '^calls_passed.126$' "$OUT/geq-parity.log" || ! grep -q '^rc=0' "$OUT/geq-parity.log"; then say "STOP parity not 126/126"; exit 1; fi
say "parity 126/126"

for step in $STEPS; do
	case $step in
	prep)
		[ -e "$CH" ] || { snap "$BOX/http-1" > "$OUT/geq-snap-http-1.before"; snap "$BOX/stream-1" > "$OUT/geq-snap-stream-1.before"; cp -al "$BOX/http-1" "$CH"; cp -al "$BOX/stream-1" "$CS"; }
		say "prep done $(cut -c1-12 "$OUT/geq-snap-http-1.before") $(cut -c1-12 "$OUT/geq-snap-stream-1.before")" ;;
	matrix)
		[ -d "$BOX/geqhome/s$DEN-all" ] || sh "$SRC/geq-subset.sh" "$DEN" "s$DEN" >> "$LOG" 2>&1
		say "matrix wait"
		L sh "$SRC/geq-matrix.sh" "$ROUNDS" "$DEN" >> "$LOG" 2>&1 || { say "STOP matrix failed"; exit 1; }
		say "matrix done" ;;
	lagS) lag lag-stream "$S" "$CS" TAG=-q2 N="${LAG_N:-10}" SPACING="${LAG_SPACING:-15}" PORT=7910 PUSH_PORT=7920 ;;
	lagH) lag lag-http "$H" "$CH" TAG=-q2 N="${LAG_N:-10}" SPACING="${LAG_SPACING:-15}" PORT=7910 ;;
	traceS) lag openat-stream "$S" "$CS" TAG=-o2 N=5 SPACING=15 STRACE_OPENAT=1 PORT=7911 PUSH_PORT=7921 ;;
	traceH) lag openat-http "$H" "$CH" TAG=-o2 N=5 SPACING=15 STRACE_OPENAT=1 PORT=7911 ;;
	sweep)
		lag sweep "$SB" "$CS" TAG=-sw2 SWEEP=1 N="${SWEEP_N:-40}" SPACING="${SWEEP_SPACING:-5}" PORT=7912 PUSH_PORT=7922 ;;
	fullH)
		say "fullH wait"
		L env PORT=7900 PROJECTED_GB=28 sh "$SRC/geq-push.sh" "$H" "$FH" >> "$LOG" 2>&1 || { say "STOP fullH failed"; exit 1; }
		say "fullH done" ;;
	fullS)
		say "fullS wait"
		P=$(awk -F'\t' '$1=="puts"{print $2}' "$OUT/geq-geqf-http-1-summary.txt")
		L env PORT=7901 PROJECTED_GB=26 APPENDS="$P" sh "$SRC/geq-push.sh" "$S" "$FS" >> "$LOG" 2>&1 || { say "STOP fullS failed"; exit 1; }
		say "fullS done"
		sh "$SRC/equiv.sh" "$BOX/http-1" "$FH" > "$OUT/geq-full-http-equiv.txt" 2>&1 || { say "STOP fullH not EQUIVALENT to http-1"; exit 1; }
		sh "$SRC/equiv.sh" "$BOX/http-1" "$FS" > "$OUT/geq-full-stream-equiv.txt" 2>&1 || { say "STOP fullS not EQUIVALENT to http-1"; exit 1; }
		say "full pair EQUIVALENT to http-1" ;;
	esac
done
if [ -f "$OUT/geq-snap-http-1.before" ]; then
	snap "$BOX/http-1" > "$OUT/geq-snap-http-1.after"; snap "$BOX/stream-1" > "$OUT/geq-snap-stream-1.after"
	if cmp -s "$OUT/geq-snap-http-1.before" "$OUT/geq-snap-http-1.after" && cmp -s "$OUT/geq-snap-stream-1.before" "$OUT/geq-snap-stream-1.after"; then say "http-1 and stream-1 unchanged"; else say "WARNING http-1 or stream-1 CHANGED"; fi
fi
say "driver end"
