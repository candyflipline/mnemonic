#!/usr/bin/env bash
# Shared by run.sh and e3-lab.sh, which set T, BIN, LOGS, E2E, TENANT and
# ARENA and keep every started pid in PIDS through track.
field() { tr ' ' '\n' <<<"$1" | sed -n "s/^$2=//p" | head -1; }

dead() {
	local s
	s=$(ps -o stat= -p "$1" 2>/dev/null | tr -d ' ')
	[ -z "$s" ] || [ "${s#Z}" != "$s" ]
}

track() { PIDS="$PIDS $1"; }

# start_keeper NAME STORE ROOT TTFB READERS sets KPORT, SPORT and KPID.
start_keeper() {
	local name=$1 store=$2 root=$3 ttfb=$4 readers=$5 ports uri
	ports=$("$E2E" ports 2) || return 1
	KPORT=$(sed -n 1p <<<"$ports")
	SPORT=$(sed -n 2p <<<"$ports")
	uri="file://$store?serve=http://127.0.0.1:$SPORT"
	[ -n "$ttfb" ] && uri="$uri&ttfb=$ttfb"
	mkdir -p "$store" "$T/$name/local"
	"$BIN/a" keep -tenant "$TENANT" -root "$root" -local "$T/$name/local" -store "$uri" \
		-readers "$readers" -listen "127.0.0.1:$KPORT" -grant-ttl 10m -quiet 15m -settle -interval 2s \
		>"$LOGS/$name-keeper.log" 2>&1 &
	KPID=$!
	track "$KPID"
	"$E2E" wait -addr "127.0.0.1:$KPORT" -timeout 30s
}

# start_reader NAME I starts reader rI on RPORT[I] with an empty cache dir and sets RPID[I].
start_reader() {
	local name=$1 i=$2
	rm -rf "${T:?}/$name/cache-r$i"
	"$BIN/a" read -id "r$i" -listen "127.0.0.1:${RPORT[$i]}" -cache "$T/$name/cache-r$i" \
		-keeper "http://127.0.0.1:$KPORT" -readers r1,r2,r3 >"$LOGS/$name-r$i.log" 2>&1 &
	RPID[i]=$!
	track "${RPID[$i]}"
}

# cluster_up NAME STORE ROOT TTFB: one keeper and three readers.
cluster_up() {
	local name=$1 ports
	ports=$("$E2E" ports 3) || return 1
	RPORT[1]=$(sed -n 1p <<<"$ports")
	RPORT[2]=$(sed -n 2p <<<"$ports")
	RPORT[3]=$(sed -n 3p <<<"$ports")
	READERS="r1=http://127.0.0.1:${RPORT[1]},r2=http://127.0.0.1:${RPORT[2]},r3=http://127.0.0.1:${RPORT[3]}"
	start_keeper "$name" "$2" "$3" "$4" "$READERS" || return 1
	for i in 1 2 3; do
		start_reader "$name" "$i"
	done
	for i in 1 2 3; do
		"$E2E" wait -addr "127.0.0.1:${RPORT[$i]}" -timeout 30s || return 1
	done
}

kill_readers() {
	for i in 1 2 3; do
		kill -9 "${RPID[$i]}" 2>/dev/null
		wait "${RPID[$i]}" 2>/dev/null
	done
}

drive() { "$E2E" drive -keeper "http://127.0.0.1:$KPORT" -arena "$ARENA" "$@"; }

ratio() { echo "${1:-?}/${2:-?}"; }

