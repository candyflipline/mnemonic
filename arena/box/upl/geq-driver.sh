#!/bin/sh
# geq-driver.sh — the GEQ stages in order, each under its own box lock hold, so
# other lanes interleave between stages. Not itself under the lock. Stops at once
# when parity is not 126/126 or an equiv.sh pair is not EQUIVALENT.
# Log: $OUT/geq-driver.log. Run detached: setsid nohup sh geq-driver.sh ... &
# Stages (STAGES, space separated, default all): matrix fullH fullS lag strace sweep
# Env: BOX, SRC, DEN (32), ROUNDS (5), LAG_N (8), LAG_SPACING (15), SWEEP_N (40), SWEEP_SPACING (5)
set -eu
BOX=${BOX:-/var/lib/mnu}
OUT=${OUT:-$BOX/out}
SRC=${SRC:-$BOX/geq-src}
LOCK=$BOX/box.lock
H=$BOX/base-bin/mnemonic-83b0042
S=$BOX/geq-bin/mnemonic-3829b3c
SB=$BOX/geq-bin/mnemonic-3829b3c-bump
DEN=${DEN:-32}
ROUNDS=${ROUNDS:-5}
STAGES=${STAGES:-matrix fullH fullS lag strace sweep}
LOG=$OUT/geq-driver.log
say() { echo "$(TZ=America/Los_Angeles date '+%H:%M:%S PT') $*" >> "$LOG"; }
have() { case " $STAGES " in *" $1 "*) return 0;; esac; return 1; }
FH=$BOX/geqf-http-1
FS=$BOX/geqf-stream-1

say "driver start stages=$STAGES"
until grep -q '^calls_passed' "$OUT/geq-parity.log" 2>/dev/null; do sleep 10; done
if ! grep -q '^calls_passed.126$' "$OUT/geq-parity.log" || ! grep -q '^rc=0' "$OUT/geq-parity.log"; then say "STOP parity not 126/126"; exit 1; fi
say "parity 126/126"

have matrix && {
	[ -d "$BOX/geqhome/s$DEN-all" ] || sh "$SRC/geq-subset.sh" "$DEN" "s$DEN" >> "$LOG" 2>&1
	say "matrix wait"
	flock "$LOCK" sh "$SRC/geq-matrix.sh" "$ROUNDS" "$DEN" >> "$LOG" 2>&1 || { say "STOP matrix failed"; exit 1; }
	say "matrix done"
}
have fullH && {
	say "fullH wait"
	flock "$LOCK" env PORT=7900 PROJECTED_GB=28 sh "$SRC/geq-push.sh" "$H" "$FH" >> "$LOG" 2>&1 || { say "STOP fullH failed"; exit 1; }
	say "fullH done"
}
have fullS && {
	say "fullS wait"
	P=$(awk -F'\t' '$1=="puts"{print $2}' "$OUT/geq-geqf-http-1-summary.txt")
	flock "$LOCK" env PORT=7901 PROJECTED_GB=26 APPENDS="$P" sh "$SRC/geq-push.sh" "$S" "$FS" >> "$LOG" 2>&1 || { say "STOP fullS failed"; exit 1; }
	say "fullS done"
	sh "$SRC/equiv.sh" "$BOX/http-1" "$FH" > "$OUT/geq-full-http-equiv.txt" 2>&1 || { say "STOP fullH not EQUIVALENT to http-1"; exit 1; }
	sh "$SRC/equiv.sh" "$BOX/http-1" "$FS" > "$OUT/geq-full-stream-equiv.txt" 2>&1 || { say "STOP fullS not EQUIVALENT to http-1"; exit 1; }
	say "full pair EQUIVALENT to http-1"
}
have lag && {
	for r in 1 2; do
		for side in http stream; do
			if [ $side = http ]; then BIN=$H; D=$FH; else BIN=$S; D=$FS; fi
			say "lag $side $r wait"
			flock "$LOCK" env TAG=-q$r N="${LAG_N:-8}" SPACING="${LAG_SPACING:-15}" FIRST_MAX=900 IDX_MAX=600 PORT=7910 sh "$SRC/lag.sh" "$BIN" "$D" > "$OUT/geq-lag-$side-$r.txt" 2>&1 || { say "lag $side $r rc!=0"; }
			say "lag $side $r done"
		done
	done
}
have strace && {
	for side in http stream; do
		if [ $side = http ]; then BIN=$H; D=$FH; else BIN=$S; D=$FS; fi
		say "strace $side wait"
		flock "$LOCK" env TAG=-o N=6 SPACING=15 FIRST_MAX=900 IDX_MAX=600 STRACE_OPENAT=1 PORT=7911 sh "$SRC/lag.sh" "$BIN" "$D" > "$OUT/geq-openat-$side.txt" 2>&1 || { say "openat $side rc!=0"; }
		say "strace $side done"
	done
}
have sweep && {
	say "sweep wait"
	flock "$LOCK" env TAG=-sw SWEEP=1 N="${SWEEP_N:-40}" SPACING="${SWEEP_SPACING:-5}" FIRST_MAX=900 IDX_MAX=900 PORT=7912 sh "$SRC/lag.sh" "$SB" "$FS" > "$OUT/geq-sweep.txt" 2>&1 || say "sweep rc!=0"
	say "sweep done"
}
say "driver end"
