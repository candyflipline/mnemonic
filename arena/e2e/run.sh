#!/usr/bin/env bash
# Proves done-predicate 2a-2g of traces substrate v0 and D1 on the Mac against
# the fixture: a file store process, one keeper, one router, three readers, every
# call through the router. Prints exactly seven lines on stdout, 2a: to 2g:, and
# everything else on stderr.
# Exits non-zero if any fails. The caller holds heavy.lock:
#   lockf -k /Users/user/xstock-scratch/heavy.lock nice -n 15 env GOMAXPROCS=4 GOMEMLIMIT=900MiB arena/e2e/run.sh
# KEEP=1 keeps the temp dir; ONLY="f g" runs those steps. MAX_CALL_S (default 5) is the keeper's -max-call in seconds. ARENA and SRC override the fixture and the
# transcripts the seed reads. BIN_DIR=<dir holding a, e2e, seed> skips the build. LOG_DIR keeps the logs
# and du.txt outside the temp dir. ENVLABEL names the host and corpus in the 2e line. WAIT_UP_S,
# WAIT_EXIT_S and PREWARM_WAIT_S (seconds) and E2E_TIMEOUT (a duration) stretch the waits for a big corpus.
set -u -o pipefail
exec 3>&1 1>&2

ARENA=${ARENA:-/Users/user/loyal/mnemonic-harness/arena-fixture}
SRC=${SRC:-/Users/user/.claude/jobs/47d8265e/tmp/sample2}
BUILD_LOCK=${BUILD_LOCK:-/Users/user/xstock-scratch/build.lock}
export GOMAXPROCS=${GOMAXPROCS:-4} GOMEMLIMIT=${GOMEMLIMIT:-900MiB}
TENANT=e2e
TTFB=50ms

for set in golden hidden; do
	[ -s "$ARENA/$set/calls.jsonl" ] || { echo "$ARENA/$set/calls.jsonl missing or empty" >&2; exit 2; }
done
EXPECT=$(cat "$ARENA/golden/calls.jsonl" "$ARENA/hidden/calls.jsonl" | grep -c .)
ENVLABEL=${ENVLABEL:-Mac, fixture}
export WAIT_UP_S=${WAIT_UP_S:-30} WAIT_EXIT_S=${WAIT_EXIT_S:-45} PREWARM_WAIT_S=${PREWARM_WAIT_S:-120}

T=$(mktemp -d "${TMPDIR:-/tmp}/e2e.XXXXXX")
BIN=${BIN_DIR:-$T/bin}
LOGS=${LOG_DIR:-$T/log}
mkdir -p "$BIN" "$LOGS"
FAILED=0
PIDS=""

# shellcheck disable=SC2329
cleanup() {
	trap - EXIT INT TERM
	for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
	for p in $PIDS; do wait "$p" 2>/dev/null; done
	if [ -n "${KEEP:-}" ]; then echo "kept $T" >&2; else rm -rf "$T"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

build() {
	local REPO
	REPO=$(cd "$(dirname "$0")/../.." && pwd) || return 1
	(cd "$REPO" && lockf -k "$BUILD_LOCK" nice -n 15 env GOMAXPROCS=4 GOFLAGS=-p=2 CGO_ENABLED=0 go build -o "$1" "$2")
}

for target in a e2e seed; do
	if [ -n "${BIN_DIR:-}" ]; then
		[ -x "$BIN/$target" ] || {
			echo "$BIN/$target is not an executable" >&2
			exit 2
		}
		continue
	fi
	build "$BIN/$target" "./arena/$target" || {
		echo "build $target failed" >&2
		exit 1
	}
done
E2E=$BIN/e2e
# shellcheck source=./lib.sh
. "$(dirname "$0")/lib.sh"

emit() { echo "$*" >&3; }
fail() {
	FAILED=1
	emit "$1: FAIL $2"
}

why() {
	local m
	m=$(grep -h -m1 -E 'panic|lane L[0-9]' "$LOGS"/"$1"-*.log 2>/dev/null | head -1 | cut -c1-120)
	[ -z "$m" ] && m=$(tail -n 1 "$LOGS/$1-keeper.log" 2>/dev/null | cut -c1-120)
	echo "${m:-no log}"
}

kb() { du -sk "$@" 2>/dev/null | awk '{s+=$1} END{print s+0}'; }

# step_du S appends the disk each dir of step S holds, in KiB, to $LOGS/du.txt before it is dropped.
step_du() {
	local d
	for d in "$T"/2"$1"*; do
		[ -d "$d" ] || continue
		echo "$(basename "$d") store_kb=$(kb "$d/store") root_kb=$(kb "$d/root") local_kb=$(kb "$d/local") cache_kb=$(kb "$d"/cache-r*)" >>"$LOGS/du.txt"
	done
}

# kill_all stops every process the steps started.
kill_all() {
	for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
	for p in $PIDS; do wait "$p" 2>/dev/null; done
	PIDS=""
}

# retire_step S stops what the step started and drops its dirs, so a run's disk use is one step's.
retire_step() {
	step_du "$1"
	kill_all
	[ -n "${KEEP:-}" ] || rm -rf "$T"/2"$1"*
}

full() { [ "$(field "$1" total)" = "$EXPECT" ] && [ "$(field "$1" equal)" = "$EXPECT" ]; }

s2a() {
	local out_w out_p ok=1 note=""
	cluster_up 2a "$T/2a/store" "$ARENA/raw" "" || { fail 2a "cluster did not start ($(why 2a))"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { fail 2a "ingest failed ($(why 2a))"; return; }
	out_w=$(drive -rung whole -save "$T/clean") || ok=0
	out_p=$(drive -rung partial) || ok=0
	full "$out_w" || ok=0
	full "$out_p" || ok=0
	[ $ok = 0 ] && note=" ($(why 2a))"
	local line
	line="whole $(ratio "$(field "$out_w" equal)" "$(field "$out_w" total)") partial $(ratio "$(field "$out_p" equal)" "$(field "$out_p" total)")"
	if [ $ok = 1 ]; then emit "2a: PASS $line"; else fail 2a "$line$note"; fi
}

s2b() {
	local out ok=1 victim kill_at=$((EXPECT / 2))
	cluster_up 2b "$T/2b/store" "$ARENA/raw" "" || { fail 2b "cluster did not start ($(why 2b))"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { fail 2b "ingest failed ($(why 2b))"; return; }
	victim=${RPID[2]}
	out=$(drive -rung partial -kill-pid "$victim" -kill-at "$kill_at") || ok=0
	dead "$victim" || { ok=0; echo "reader r2 is still alive after the kill" >&2; }
	full "$out" || ok=0
	local line
	line="$(ratio "$(field "$out" equal)" "$(field "$out" total)") partial, reader r2 SIGKILLed before call $kill_at"
	if [ $ok = 1 ]; then emit "2b: PASS $line"; else fail 2b "$line ($(why 2b))"; fi
}

manifests_at() { ls "$1/t/$TENANT/m" | grep -c "^$(printf %016x "$2")-"; }

# wait_exit PID waits up to WAIT_EXIT_S (45) s for PID to exit and sets RC to its status.
wait_exit() {
	local n=0
	RC=none
	while [ $n -lt $((WAIT_EXIT_S * 2)) ] && ! dead "$1"; do
		sleep 0.5
		n=$((n + 1))
	done
	if dead "$1"; then
		wait "$1"
		RC=$?
	fi
}

# stale_commit: keeper A (gen g1) commits the half root, keeper B takes over, then more raw arrives and A is asked to ingest.
stale_commit() {
	local store=$T/2c/store root=$T/2c/root a g1 g2 out before
	"$BIN/seed" -src "$SRC" -half "$root" >/dev/null || { STALE="seed -half failed"; return 1; }
	start_store 2c "$store" || { STALE="store did not start ($(why 2c))"; return 1; }
	start_keeper 2c "$root" || { STALE="first keeper did not start ($(why 2c))"; return 1; }
	a=$KPID
	local a_port=$KPORT
	"$E2E" ingest -keeper "http://127.0.0.1:$a_port" || { STALE="first keeper ingest failed ($(why 2c))"; return 1; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT") || { STALE="first manifest: $out"; return 1; }
	g1=$(field "$out" gen)
	start_keeper 2c2 "$root" || { STALE="second keeper did not start ($(why 2c2))"; return 1; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { STALE="second keeper ingest failed ($(why 2c2))"; return 1; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT") || { STALE="second manifest: $out"; return 1; }
	g2=$(field "$out" gen)
	[ "${g2:-0}" -gt "${g1:-0}" ] || { STALE="second keeper did not take a higher gen ($g1 then $g2)"; return 1; }
	before=$(manifests_at "$store" "$g1")
	"$BIN/seed" -src "$SRC" -rest "$root" >/dev/null || { STALE="seed -rest failed"; return 1; }
	"$E2E" ingest -keeper "http://127.0.0.1:$a_port" || echo "stale keeper refused the ingest" >&2
	wait_exit "$a"
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { STALE="second keeper ingest after the stale commit failed ($(why 2c2))"; return 1; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT" -gen "$g2") || { STALE="after the stale keeper: $out"; return 1; }
	[ "$(manifests_at "$store" "$g1")" = "$before" ] || { STALE="the stale keeper's commit landed a gen $g1 manifest"; return 1; }
	[ "$RC" = 3 ] || { STALE="stale keeper exit status $RC, want 3 ($(why 2c))"; return 1; }
	STALE="commit: gen $g1 keeper exit 3, no gen $g1 manifest after the takeover"
}

# stale_delete: keeper A (gen g1, grants of 3 s, cycles only when asked) commits twice, so its first manifest is queued for deletion; keeper B takes over before it is due; asked to cycle once it is due, A's delete must be refused.
stale_delete() {
	local store=$T/2cd/store root=$T/2cd/root a out g1 g2 seq=0 n=0
	"$BIN/seed" -src "$SRC" -half "$root" >/dev/null || { STALE="seed -half failed"; return 1; }
	start_store 2cd "$store" || { STALE="store did not start ($(why 2cd))"; return 1; }
	start_keeper 2cd "$root" "-grant-ttl 3s -delete-slack 3s -interval 1h" || { STALE="delete keeper A did not start ($(why 2cd))"; return 1; }
	a=$KPID
	local a_port=$KPORT
	"$E2E" ingest -keeper "http://127.0.0.1:$a_port" >/dev/null || { STALE="delete keeper A ingest failed ($(why 2cd))"; return 1; }
	"$BIN/seed" -src "$SRC" -rest "$root" >/dev/null || { STALE="seed -rest failed"; return 1; }
	while [ "${seq:-0}" -lt 2 ] && [ $n -lt 5 ]; do
		out=$("$E2E" ingest -keeper "http://127.0.0.1:$a_port") || { STALE="delete keeper A second ingest failed ($(why 2cd))"; return 1; }
		seq=$(field "$out" seq)
		n=$((n + 1))
	done
	[ "${seq:-0}" -ge 2 ] || { STALE="delete keeper A never reached seq 2"; return 1; }
	g1=$(field "$out" gen)
	spawn_keeper 2cd2 "$root" || { STALE="delete keeper B did not spawn"; return 1; }
	local b=$KPID b_port=$KPORT
	n=0
	while [ "$(ls "$store/t/$TENANT/g" | wc -l)" -lt 2 ] && [ $n -lt 100 ]; do
		sleep 0.1
		n=$((n + 1))
	done
	sleep "$((6 + MAX_CALL_S)).5"
	"$E2E" ingest -keeper "http://127.0.0.1:$a_port" || echo "stale keeper refused the ingest" >&2
	wait_up "$b" "127.0.0.1:$b_port" || { STALE="delete keeper B did not start ($(why 2cd2))"; return 1; }
	out=$("$E2E" ingest -keeper "http://127.0.0.1:$b_port") || { STALE="delete keeper B ingest failed ($(why 2cd2))"; return 1; }
	g2=$(field "$out" gen)
	[ "${g2:-0}" -gt "${g1:-0}" ] || { STALE="delete keeper B did not take a higher gen ($g1 then $g2)"; return 1; }
	wait_exit "$a"
	grep -qE 'keep: delete refused|keep: gen [0-9]+ lost to a newer keeper' "$LOGS/2cd-keeper.log" || { STALE="neither 'delete refused' nor 'lost to a newer keeper' in keeper A's log (exit $RC)"; return 1; }
	how=$(grep -q 'keep: delete refused' "$LOGS/2cd-keeper.log" && echo "due delete refused" || echo "gen check fenced it before any delete came due")
	[ "$RC" = 3 ] || { STALE="delete keeper A exit status $RC, want 3"; return 1; }
	[ -e "$store/t/$TENANT/m/$(printf %016x "$g1")-$(printf %016x 1).json" ] || { STALE="the manifest keeper A queued for deletion is gone"; return 1; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT" -gen "$g2") || { STALE="after the refused delete: $out"; return 1; }
	STALE="delete: gen $g1 keeper's $how, exit 3, its queued manifest still there"
}

s2c() {
	local c d
	stale_commit && c=1 || c=0
	local cmsg=$STALE
	stale_delete && d=1 || d=0
	if [ $c = 1 ] && [ $d = 1 ]; then
		emit "2c: PASS stale keeper $cmsg; stale keeper $STALE; latest manifest stays the new keeper's, 0 named objects missing"
	else
		[ $c = 0 ] && fail 2c "stale commit: $cmsg" || fail 2c "stale delete: $STALE"
	fi
}

s2d() {
	local root=$T/2d/root out_p out_w ok=1 base=""
	[ -d "$T/clean" ] && base="-baseline $T/clean"
	"$BIN/seed" -src "$SRC" -half "$root" >/dev/null || { fail 2d "seed -half failed"; return; }
	cluster_up 2d "$T/2d/store" "$root" "" || { fail 2d "cluster did not start ($(why 2d))"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { fail 2d "half ingest failed ($(why 2d))"; return; }
	"$BIN/seed" -src "$SRC" -rest "$root" >/dev/null || { fail 2d "seed -rest failed"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { fail 2d "rest ingest failed ($(why 2d))"; return; }
	# shellcheck disable=SC2086
	out_w=$(drive -rung whole $base) || ok=0
	# shellcheck disable=SC2086
	out_p=$(drive -rung partial $base) || ok=0
	full "$out_w" || ok=0
	full "$out_p" || ok=0
	local what="a clean run"
	[ -z "$base" ] && what="the expected results (no clean run saved)"
	local line
	line="half then rest, each ingested and committed: whole $(ratio "$(field "$out_w" equal)" "$(field "$out_w" total)") partial $(ratio "$(field "$out_p" equal)" "$(field "$out_p" total)") equal to $what"
	if [ $ok = 1 ]; then emit "2d: PASS $line"; else fail 2d "$line ($(why 2d))"; fi
}

# prewarm_parts LOG prints the number of parts a reader prewarmed.
prewarm_parts() { sed -n 's/.*prewarmed \([0-9]*\) parts in .*/\1/p' "$1" | head -1; }

# prewarm_ms LOG prints the join prewarm wall time a reader logged.
prewarm_ms() { sed -n 's/.*prewarmed [0-9]* parts in \([0-9]*\)ms since joining.*/\1/p' "$1" | head -1; }

# s2e_at TTFB runs one cluster behind a file store with that first-byte delay ("" is none) and sets E2E_OK, E2E_TOTAL and E2E_LINE.
# The join prewarm is measured with default readers. Each tool is then measured with fresh readers started -prewarm=false, so its first calls fetch on demand and the same calls again are warm.
s2e_at() {
	local ttfb=$1 label=${1:-0} stats_url tool base0 pre out i n ms max_ms=0 cold warm tools split=""
	E2E_OK=1
	E2E_TOTAL=0
	E2E_LINE="ttfb=${label}:"
	cluster_up "2e$label" "$T/2e$label/store" "$ARENA/raw" "$ttfb" "" || { E2E_OK=0; E2E_LINE="$E2E_LINE cluster did not start ($(why "2e$label"))"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { E2E_OK=0; E2E_LINE="$E2E_LINE ingest failed"; return; }
	stats_url="http://127.0.0.1:$SPORT/stats"
	tools=$("$E2E" tools -arena "$ARENA") || { E2E_OK=0; E2E_LINE="$E2E_LINE no tools"; return; }
	kill_readers
	base0=$("$E2E" stats -url "$stats_url")
	for i in 1 2 3; do
		start_reader "2e$label" "$i"
	done
	for i in 1 2 3; do
		wait_up "${RPID[$i]}" "127.0.0.1:${RPORT[$i]}" || E2E_OK=0
	done
	n=0
	while [ $n -lt $((PREWARM_WAIT_S * 2)) ]; do
		[ -n "$(prewarm_ms "$LOGS/2e$label-r1.log")" ] && [ -n "$(prewarm_ms "$LOGS/2e$label-r2.log")" ] && [ -n "$(prewarm_ms "$LOGS/2e$label-r3.log")" ] && break
		sleep 0.5
		n=$((n + 1))
	done
	for i in 1 2 3; do
		ms=$(prewarm_ms "$LOGS/2e$label-r$i.log")
		[ -z "$ms" ] && { E2E_OK=0; ms=0; }
		[ "$ms" -gt "$max_ms" ] && max_ms=$ms
		split="$split${split:+/}$(prewarm_parts "$LOGS/2e$label-r$i.log")"
	done
	pre=$("$E2E" stats -url "$stats_url")
	E2E_LINE="$E2E_LINE join prewarm $(($(field "$pre" bytes) - $(field "$base0" bytes)))B/$(($(field "$pre" requests) - $(field "$base0" requests)))req/${max_ms}ms (3 readers in parallel, parts per reader $split);"
	for tool in $tools; do
		kill_readers
		for i in 1 2 3; do
			start_reader "2e$label" "$i" -prewarm=false
		done
		for i in 1 2 3; do
			wait_up "${RPID[$i]}" "127.0.0.1:${RPORT[$i]}" || E2E_OK=0
		done
		out=$(drive -rung partial -tool "$tool" -passes 2 -stats "$stats_url") || E2E_OK=0
		cold=$(sed -n 1p <<<"$out")
		warm=$(sed -n 2p <<<"$out")
		E2E_TOTAL=$((E2E_TOTAL + $(field "$cold" total)))
		E2E_LINE="$E2E_LINE $tool cold $(field "$cold" bytes)B/$(field "$cold" requests)req p50 $(field "$cold" p50_ms)ms (fetching calls $(field "$cold" fetched_calls) of $(field "$cold" total), their p50 $(field "$cold" fetched_p50_ms)ms max $(field "$cold" fetched_max_ms)ms) warm $(field "$warm" bytes)B/$(field "$warm" requests)req p50 $(field "$warm" p50_ms)ms;"
		if [ "$tool" = get_ingest_status ]; then
			[ "$(field "$cold" requests)" = 0 ] || E2E_OK=0
		else
			[ "$(field "$cold" requests)" -gt 0 ] 2>/dev/null || E2E_OK=0
			[ "$(field "$cold" bytes)" -gt 0 ] 2>/dev/null || E2E_OK=0
		fi
		[ "$(field "$warm" requests)" = 0 ] && [ "$(field "$warm" bytes)" = 0 ] || E2E_OK=0
	done
	[ "$E2E_TOTAL" = "$EXPECT" ] || E2E_OK=0
	kill_readers
}

s2e() {
	local ok=1 line="$ENVLABEL, partial rung, 3 readers, cold = fresh cache and -prewarm=false; get_ingest_status is router-answered so its 0 is by design;"
	for ttfb in "$TTFB" ""; do
		s2e_at "$ttfb"
		[ "$E2E_OK" = 1 ] || ok=0
		line="$line $E2E_LINE"
		retire_step e
	done
	if [ $ok = 1 ]; then emit "2e: PASS $line"; else fail 2e "$line ($(why 2e0))"; fi
}

# ingest_counts OUT prints OUT's four outcome counts.
ingest_counts() { echo "unchanged=$(field "$1" unchanged) resumed=$(field "$1" resumed) fresh=$(field "$1" fresh) reparsed=$(field "$1" reparsed)"; }

kill_keeper() {
	kill -9 "$1" 2>/dev/null
	wait "$1" 2>/dev/null
}

# s2f_first: keeper A ingests the settled raw and is killed; keeper B on an empty local over the same store parses nothing, commits at a higher gen naming the same part keys, and the cluster answers both rungs N/N.
s2f_first() {
	local out un sources g1 g2 keys1 keys2 m1 m2 ok=1 w p store=$T/2f/store
	cluster_up 2f "$store" "$ARENA/raw" "" || { F_MSG="cluster did not start ($(why 2f))"; return 1; }
	local a=$KPID
	out=$("$E2E" ingest -keeper "http://127.0.0.1:$KPORT") || { F_MSG="keeper A ingest failed ($(why 2f))"; return 1; }
	sources=$(field "$out" fresh)
	{ [ "${sources:-0}" -gt 0 ] && [ "$(field "$out" resumed)" = 0 ] && [ "$(field "$out" reparsed)" = 0 ]; } || { F_MSG="keeper A first ingest did not parse every source fresh ($(ingest_counts "$out"))"; return 1; }
	m1=$("$E2E" manifest -dir "$store" -tenant "$TENANT") || { F_MSG="manifest after keeper A: $m1"; return 1; }
	g1=$(field "$m1" gen)
	keys1=$(field "$m1" keys)
	kill_keeper "$a"
	respawn_keeper 2f2 "$ARENA/raw" || { F_MSG="keeper B did not spawn"; return 1; }
	wait_up "$KPID" "127.0.0.1:$KPORT" || { F_MSG="keeper B did not start ($(why 2f2))"; return 1; }
	out=$("$E2E" ingest -keeper "http://127.0.0.1:$KPORT") || { F_MSG="keeper B ingest failed ($(why 2f2))"; return 1; }
	un=$(field "$out" unchanged)
	{ [ "${un:-0}" -gt 0 ] && [ $((un % sources)) = 0 ] && [ "$(field "$out" resumed)" = 0 ] && [ "$(field "$out" fresh)" = 0 ] && [ "$(field "$out" reparsed)" = 0 ]; } || { F_MSG="keeper B on an empty local parsed: $(ingest_counts "$out"), want resumed=0 fresh=0 reparsed=0 and unchanged a multiple of $sources"; return 1; }
	m2=$("$E2E" manifest -dir "$store" -tenant "$TENANT") || { F_MSG="manifest after keeper B: $m2"; return 1; }
	g2=$(field "$m2" gen)
	keys2=$(field "$m2" keys)
	[ "${g2:-0}" -gt "${g1:-0}" ] || { F_MSG="keeper B did not commit at a higher gen ($g1 then $g2)"; return 1; }
	{ [ -n "$keys1" ] && [ "$keys1" = "$keys2" ]; } || { F_MSG="keeper B's manifest names other part keys than keeper A's"; return 1; }
	w=$(drive -rung whole) || ok=0
	p=$(drive -rung partial) || ok=0
	full "$w" || ok=0
	full "$p" || ok=0
	F_MSG="whole $(ratio "$(field "$w" equal)" "$(field "$w" total)") partial $(ratio "$(field "$p" equal)" "$(field "$p" total)")"
	[ $ok = 1 ] || { F_MSG="$F_MSG ($(why 2f2))"; return 1; }
	F_MSG="keeper B on an empty local over keeper A's store: $(ingest_counts "$out") ($sources sources), gen $g1 to $g2 naming the same part keys, $F_MSG"
}

# s2f_half: keeper C ingests the half root and is killed, more raw arrives, keeper D on an empty local over C's store ingests it without a reparse, and the cluster is N/N equal to a clean run.
s2f_half() {
	local root=$T/2fh/root out base="" ok=1 w p what="a clean run"
	[ -d "$T/clean" ] && base="-baseline $T/clean"
	[ -z "$base" ] && what="the expected results (no clean run saved)"
	"$BIN/seed" -src "$SRC" -half "$root" >/dev/null || { H_MSG="seed -half failed"; return 1; }
	cluster_up 2fh "$T/2fh/store" "$root" "" || { H_MSG="cluster did not start ($(why 2fh))"; return 1; }
	local c=$KPID
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { H_MSG="keeper C ingest failed ($(why 2fh))"; return 1; }
	kill_keeper "$c"
	"$BIN/seed" -src "$SRC" -rest "$root" >/dev/null || { H_MSG="seed -rest failed"; return 1; }
	respawn_keeper 2fh2 "$root" || { H_MSG="keeper D did not spawn"; return 1; }
	wait_up "$KPID" "127.0.0.1:$KPORT" || { H_MSG="keeper D did not start ($(why 2fh2))"; return 1; }
	out=$("$E2E" ingest -keeper "http://127.0.0.1:$KPORT") || { H_MSG="keeper D ingest failed ($(why 2fh2))"; return 1; }
	[ "$(field "$out" reparsed)" = 0 ] || { H_MSG="keeper D reparsed sources: $(ingest_counts "$out")"; return 1; }
	# shellcheck disable=SC2086
	w=$(drive -rung whole $base) || ok=0
	# shellcheck disable=SC2086
	p=$(drive -rung partial $base) || ok=0
	full "$w" || ok=0
	full "$p" || ok=0
	H_MSG="half, keeper killed, rest, fresh keeper D: $(ingest_counts "$out"), whole $(ratio "$(field "$w" equal)" "$(field "$w" total)") partial $(ratio "$(field "$p" equal)" "$(field "$p" total)") equal to $what"
	[ $ok = 1 ] || { H_MSG="$H_MSG ($(why 2fh2))"; return 1; }
}

s2f() {
	local f h fmsg
	s2f_first && f=1 || f=0
	fmsg=$F_MSG
	retire_step f
	s2f_half && h=1 || h=0
	if [ $f = 1 ] && [ $h = 1 ]; then
		emit "2f: PASS $fmsg; $H_MSG"
	elif [ $f = 0 ]; then
		fail 2f "empty-local keeper: $fmsg"
	else
		fail 2f "half then rest: $H_MSG"
	fi
}

# s2g: reads with the keeper down. The router answers committed while keeper A is dead, refuses the whole rung, keeps answering while keeper B restores, and covers everything once B has committed.
s2g() {
	local out g1 g2 ok=1 kill_at=$((EXPECT / 2)) a k w r n line
	cluster_up 2g "$T/2g/store" "$ARENA/raw" "" || { fail 2g "cluster did not start ($(why 2g))"; return; }
	a=$KPID
	out=$("$E2E" ingest -keeper "http://127.0.0.1:$KPORT") || { fail 2g "ingest failed ($(why 2g))"; return; }
	g1=$(field "$out" gen)
	k=$(drive -rung partial -kill-pid "$a" -kill-at "$kill_at") || ok=0
	dead "$a" || { ok=0; echo "keeper A is still alive after the kill" >&2; }
	full "$k" || ok=0
	[ "$(field "$k" committed)" -ge "$kill_at" ] 2>/dev/null || ok=0
	w=$(drive -rung whole -tool search_sessions -limit 1) && ok=0
	[ "$(field "$w" s503)" = 1 ] || ok=0
	respawn_keeper 2g2 "$ARENA/raw" || { fail 2g "keeper B did not spawn"; return; }
	r=$(drive -rung partial) || ok=0
	full "$r" || ok=0
	wait_up "$KPID" "127.0.0.1:$KPORT" || { fail 2g "keeper B did not start ($(why 2g2))"; return; }
	out=$("$E2E" ingest -keeper "http://127.0.0.1:$KPORT") || { fail 2g "keeper B ingest failed ($(why 2g2))"; return; }
	g2=$(field "$out" gen)
	[ "${g2:-0}" -gt "${g1:-0}" ] || ok=0
	n=$(drive -rung partial) || ok=0
	full "$n" || ok=0
	[ "$(field "$n" all)" = "$EXPECT" ] || ok=0
	line="keeper SIGKILLed before call $kill_at: partial $(ratio "$(field "$k" equal)" "$(field "$k" total)") with committed=$(field "$k" committed); whole answers 503; keeper B restoring: partial $(ratio "$(field "$r" equal)" "$(field "$r" total)") with all=$(field "$r" all) committed=$(field "$r" committed); after its first commit (gen $g1 to $g2): partial $(ratio "$(field "$n" equal)" "$(field "$n" total)") all=$(field "$n" all)"
	if [ $ok = 1 ]; then emit "2g: PASS $line"; else fail 2g "$line ($(why 2g2))"; fi
}

for s in a b c d e f g; do
	t0=$SECONDS
	case " ${ONLY:-a b c d e f g} " in *" $s "*) "s2$s" ;; esac
	retire_step "$s"
	echo "2$s took $((SECONDS - t0))s" | tee -a "$LOGS/steps.txt"
done
exit $FAILED
