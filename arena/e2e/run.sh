#!/usr/bin/env bash
# Proves done-predicate 2a-2e of traces substrate v0 on the Mac against the
# fixture: a file store served over HTTP, one keeper, three readers. Prints
# exactly five lines on stdout, 2a: to 2e:, and everything else on stderr.
# Exits non-zero if any fails. The caller holds heavy.lock:
#   lockf -k /Users/user/xstock-scratch/heavy.lock nice -n 15 env GOMAXPROCS=4 GOMEMLIMIT=900MiB arena/e2e/run.sh
# KEEP=1 keeps the temp dir; ONLY="c d" runs those steps. ARENA and SRC override the fixture and the
# transcripts the seed reads.
set -u -o pipefail
exec 3>&1 1>&2

REPO=$(cd "$(dirname "$0")/../.." && pwd)
ARENA=${ARENA:-/Users/user/loyal/mnemonic-harness/arena-fixture}
SRC=${SRC:-/Users/user/.claude/jobs/47d8265e/tmp/sample2}
BUILD_LOCK=${BUILD_LOCK:-/Users/user/xstock-scratch/build.lock}
export GOMAXPROCS=${GOMAXPROCS:-4} GOMEMLIMIT=${GOMEMLIMIT:-900MiB}
EXPECT=126
TENANT=e2e
TTFB=50ms

T=$(mktemp -d "${TMPDIR:-/tmp}/e2e.XXXXXX")
BIN=$T/bin
LOGS=$T/log
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
	(cd "$REPO" && lockf -k "$BUILD_LOCK" nice -n 15 env GOMAXPROCS=4 GOFLAGS=-p=2 CGO_ENABLED=0 go build -o "$1" "$2")
}

for target in a e2e seed; do
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

# retire_step S stops what the step started and drops its dirs, so a run's disk use is one step's.
retire_step() {
	for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
	for p in $PIDS; do wait "$p" 2>/dev/null; done
	PIDS=""
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

# wait_exit PID waits up to 45 s for PID to exit and sets RC to its status.
wait_exit() {
	local n=0
	RC=none
	while [ $n -lt 90 ] && ! dead "$1"; do
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
	start_keeper 2c "$store" "$root" "" "" || { STALE="first keeper did not start ($(why 2c))"; return 1; }
	a=$KPID
	local a_port=$KPORT
	"$E2E" ingest -keeper "http://127.0.0.1:$a_port" || { STALE="first keeper ingest failed ($(why 2c))"; return 1; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT") || { STALE="first manifest: $out"; return 1; }
	g1=$(field "$out" gen)
	start_keeper 2c2 "$store" "$root" "" "" || { STALE="second keeper did not start ($(why 2c2))"; return 1; }
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
	start_keeper 2cd "$store" "$root" "" "" "-grant-ttl 3s -delete-slack 0s -interval 1h" || { STALE="delete keeper A did not start ($(why 2cd))"; return 1; }
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
	spawn_keeper 2cd2 "$store" "$root" "" "" || { STALE="delete keeper B did not spawn"; return 1; }
	local b=$KPID b_port=$KPORT
	n=0
	while [ "$(ls "$store/t/$TENANT/g" | wc -l)" -lt 2 ] && [ $n -lt 100 ]; do
		sleep 0.1
		n=$((n + 1))
	done
	sleep 3.5
	"$E2E" ingest -keeper "http://127.0.0.1:$a_port" || echo "stale keeper refused the ingest" >&2
	wait_up "$b" "127.0.0.1:$b_port" || { STALE="delete keeper B did not start ($(why 2cd2))"; return 1; }
	out=$("$E2E" ingest -keeper "http://127.0.0.1:$b_port") || { STALE="delete keeper B ingest failed ($(why 2cd2))"; return 1; }
	g2=$(field "$out" gen)
	[ "${g2:-0}" -gt "${g1:-0}" ] || { STALE="delete keeper B did not take a higher gen ($g1 then $g2)"; return 1; }
	wait_exit "$a"
	grep -q 'keep: delete refused' "$LOGS/2cd-keeper.log" || { STALE="no 'delete refused' line in keeper A's log (exit $RC)"; return 1; }
	[ "$RC" = 3 ] || { STALE="delete keeper A exit status $RC, want 3"; return 1; }
	[ -e "$store/t/$TENANT/m/$(printf %016x "$g1")-$(printf %016x 1).json" ] || { STALE="the manifest keeper A queued for deletion is gone"; return 1; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT" -gen "$g2") || { STALE="after the refused delete: $out"; return 1; }
	STALE="delete: gen $g1 keeper's due delete refused, exit 3, its queued manifest still there"
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

stable_stats() {
	local url=$1 prev cur n=0
	prev=$("$E2E" stats -url "$url")
	while [ $n -lt 20 ]; do
		sleep 1
		cur=$("$E2E" stats -url "$url")
		[ "$cur" = "$prev" ] && return 0
		prev=$cur
		n=$((n + 1))
	done
}

s2e() {
	local stats_url tool base0 pre out ok=1 total=0 line="Mac, fixture, ttfb=$TTFB, partial rung:" tools cold warm
	cluster_up 2e "$T/2e/store" "$ARENA/raw" "$TTFB" || { fail 2e "cluster did not start ($(why 2e))"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { fail 2e "ingest failed ($(why 2e))"; return; }
	stats_url="http://127.0.0.1:$SPORT/stats"
	tools=$("$E2E" tools -arena "$ARENA") || { fail 2e "no tools"; return; }
	for tool in $tools; do
		kill_readers
		base0=$("$E2E" stats -url "$stats_url")
		for i in 1 2 3; do
			start_reader 2e "$i"
		done
		for i in 1 2 3; do
			wait_up "${RPID[$i]}" "127.0.0.1:${RPORT[$i]}" || ok=0
		done
		stable_stats "$stats_url"
		pre=$("$E2E" stats -url "$stats_url")
		out=$(drive -rung partial -tool "$tool" -passes 2 -stats "$stats_url") || ok=0
		cold=$(sed -n 1p <<<"$out")
		warm=$(sed -n 2p <<<"$out")
		total=$((total + $(field "$cold" total)))
		line="$line $tool prewarm $(($(field "$pre" bytes) - $(field "$base0" bytes)))B/$(($(field "$pre" requests) - $(field "$base0" requests)))req"
		line="$line cold $(field "$cold" bytes)B/$(field "$cold" requests)req/$(field "$cold" p50_ms)ms warm $(field "$warm" bytes)B/$(field "$warm" requests)req/$(field "$warm" p50_ms)ms;"
	done
	[ "$total" = "$EXPECT" ] || ok=0
	if [ $ok = 1 ]; then emit "2e: PASS $line"; else fail 2e "$line ($(why 2e))"; fi
}

for s in a b c d e; do
	case " ${ONLY:-a b c d e} " in *" $s "*) "s2$s" ;; esac
	retire_step "$s"
done
exit $FAILED
