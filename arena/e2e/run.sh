#!/usr/bin/env bash
# Proves done-predicate 2a-2e of traces substrate v0 on the Mac against the
# fixture: a file store served over HTTP, one keeper, three readers. Prints
# exactly five lines on stdout, 2a: to 2e:, and everything else on stderr.
# Exits non-zero if any fails. The caller holds heavy.lock:
#   lockf -k /Users/user/xstock-scratch/heavy.lock nice -n 15 env GOMAXPROCS=4 GOMEMLIMIT=900MiB arena/e2e/run.sh
# KEEP=1 keeps the temp dir. ARENA and SRC override the fixture and the
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
	m=$(grep -h -m1 '^panic:' "$LOGS"/"$1"-*.log 2>/dev/null | head -1)
	[ -z "$m" ] && m=$(tail -n 1 "$LOGS/$1-keeper.log" 2>/dev/null | cut -c1-160)
	echo "${m:-no log}"
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

s2c() {
	local store=$T/2c/store root=$T/2c/root k1 k1_port g1 g2 out rc=none n
	"$BIN/seed" -src "$SRC" -half "$root" >/dev/null || { fail 2c "seed -half failed"; return; }
	cluster_up 2c "$store" "$root" "" || { fail 2c "cluster did not start ($(why 2c))"; return; }
	k1=$KPID
	k1_port=$KPORT
	"$E2E" ingest -keeper "http://127.0.0.1:$k1_port" || { fail 2c "first keeper ingest failed ($(why 2c))"; return; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT") || { fail 2c "first keeper manifest check failed: $out"; return; }
	g1=$(field "$out" gen)
	start_keeper 2c2 "$store" "$root" "" "$READERS" || { fail 2c "second keeper did not start ($(why 2c2))"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { fail 2c "second keeper ingest failed ($(why 2c2))"; return; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT") || { fail 2c "second keeper manifest check failed: $out"; return; }
	g2=$(field "$out" gen)
	[ "${g2:-0}" -gt "${g1:-0}" ] || { fail 2c "second keeper did not take a higher gen ($g1 then $g2)"; return; }
	"$BIN/seed" -src "$SRC" -rest "$root" >/dev/null || { fail 2c "seed -rest failed"; return; }
	"$E2E" ingest -keeper "http://127.0.0.1:$k1_port" || echo "stale keeper refused the ingest" >&2
	n=0
	while [ $n -lt 90 ] && ! dead "$k1"; do
		sleep 0.5
		n=$((n + 1))
	done
	if dead "$k1"; then
		wait "$k1"
		rc=$?
	fi
	"$E2E" ingest -keeper "http://127.0.0.1:$KPORT" || { fail 2c "second keeper ingest after the stale commit failed ($(why 2c2))"; return; }
	out=$("$E2E" manifest -dir "$store" -tenant "$TENANT" -gen "$g2") || { fail 2c "after the stale keeper: $out"; return; }
	local line="stale keeper gen $g1 exit status $rc, latest manifest stays gen $g2, 0 named objects missing"
	if [ "$rc" = 3 ]; then emit "2c: PASS $line"; else fail 2c "$line, want exit status 3 ($(why 2c))"; fi
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
			"$E2E" wait -addr "127.0.0.1:${RPORT[$i]}" -timeout 30s || ok=0
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

s2a
s2b
s2c
s2d
s2e
exit $FAILED
