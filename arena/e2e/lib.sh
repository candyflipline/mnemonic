#!/usr/bin/env bash
# Shared by run.sh, which sets T, BIN, LOGS, E2E, TENANT and ARENA and keeps
# every started pid in PIDS through track.
field() { tr ' ' '\n' <<<"$1" | sed -n "s/^$2=//p" | head -1; }

dead() {
	local s
	s=$(ps -o stat= -p "$1" 2>/dev/null | tr -d ' ')
	[ -z "$s" ] || [ "${s#Z}" != "$s" ]
}

track() { PIDS="$PIDS $1"; }

MAX_CALL_S=${MAX_CALL_S:-5}

# start_store NAME DIR [TTFB] starts `a store` over DIR with a secret of its own that every process of the step
# inherits, waits until it serves /stats, and sets SPORT, STPID and STORE_URI. TTFB delays each presigned GET.
start_store() {
	local name=$1 dir=$2 ttfb=${3:-}
	SPORT=$("$E2E" ports 1) || return 1
	STORE_URI="file://$dir?serve=http://127.0.0.1:$SPORT"
	[ -n "$ttfb" ] && STORE_URI="$STORE_URI&ttfb=$ttfb"
	ARENA_OBJ_SECRET=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
	export ARENA_OBJ_SECRET
	mkdir -p "$dir"
	"$BIN/a" store -store "$STORE_URI" >"$LOGS/$name-store.log" 2>&1 &
	STPID=$!
	track "$STPID"
	wait_up "$STPID" "127.0.0.1:$SPORT" && "$E2E" stats -url "http://127.0.0.1:$SPORT/stats" >/dev/null
}

# spawn_keeper NAME ROOT [FLAGS] starts a keeper over STORE_URI with an empty local dir named after NAME and sets KPORT and KPID
# without waiting for it to listen; FLAGS are extra keep flags, split on spaces. KEEPER_PORT fixes the port.
# start_keeper waits. respawn_keeper takes over the port of the keeper before it, which the router is pointed at.
spawn_keeper() {
	local name=$1 root=$2 flags=${3:-}
	KPORT=${KEEPER_PORT:-$("$E2E" ports 1)} || return 1
	mkdir -p "$T/$name/local"
	"$BIN/a" keep -tenant "$TENANT" -root "$root" -local "$T/$name/local" -store "$STORE_URI" \
		-listen "127.0.0.1:$KPORT" -grant-ttl 10m -max-call "${MAX_CALL_S}s" -quiet 15m -settle -interval 2s $flags \
		>"$LOGS/$name-keeper.log" 2>&1 &
	KPID=$!
	track "$KPID"
}

start_keeper() {
	spawn_keeper "$@" || return 1
	wait_up "$KPID" "127.0.0.1:$KPORT"
}

respawn_keeper() { KEEPER_PORT=$KPORT spawn_keeper "$@"; }

# start_router NAME [FLAGS] starts a router over STORE_URI, the keeper on KPORT and READERS, and sets ROPORT and ROPID.
start_router() {
	local name=$1 flags=${2:-}
	ROPORT=$("$E2E" ports 1) || return 1
	"$BIN/a" route -tenant "$TENANT" -store "$STORE_URI" -keeper "http://127.0.0.1:$KPORT" -readers "$READERS" \
		-listen "127.0.0.1:$ROPORT" $flags >"$LOGS/$name-router.log" 2>&1 &
	ROPID=$!
	track "$ROPID"
	wait_up "$ROPID" "127.0.0.1:$ROPORT"
}

# wait_up PID ADDR waits up to WAIT_UP_S (30) s for ADDR, and fails at once if PID exits.
wait_up() {
	local n=0
	while [ $n -lt "${WAIT_UP_S:-30}" ]; do
		"$E2E" wait -addr "$2" -timeout 1s 2>/dev/null && return 0
		dead "$1" && return 1
		n=$((n + 1))
	done
	return 1
}

# start_reader NAME I [FLAGS] starts reader rI on RPORT[I] with an empty cache dir and sets RPID[I]; FLAGS are extra read flags.
start_reader() {
	local name=$1 i=$2 flags=${3:-}
	rm -rf "${T:?}/$name/cache-r$i"
	"$BIN/a" read -id "r$i" -listen "127.0.0.1:${RPORT[$i]}" -cache "$T/$name/cache-r$i" \
		-router "http://127.0.0.1:$ROPORT" -readers r1,r2,r3 $flags >"$LOGS/$name-r$i.log" 2>&1 &
	RPID[i]=$!
	track "${RPID[$i]}"
}

# cluster_up NAME STORE_DIR ROOT TTFB: a store, one keeper, one router and three readers.
cluster_up() {
	local name=$1 ports
	ports=$("$E2E" ports 3) || return 1
	RPORT[1]=$(sed -n 1p <<<"$ports")
	RPORT[2]=$(sed -n 2p <<<"$ports")
	RPORT[3]=$(sed -n 3p <<<"$ports")
	READERS="r1=http://127.0.0.1:${RPORT[1]},r2=http://127.0.0.1:${RPORT[2]},r3=http://127.0.0.1:${RPORT[3]}"
	start_store "$name" "$2" "$4" || return 1
	start_keeper "$name" "$3" || return 1
	start_router "$name" || return 1
	for i in 1 2 3; do
		start_reader "$name" "$i"
	done
	for i in 1 2 3; do
		wait_up "${RPID[$i]}" "127.0.0.1:${RPORT[$i]}" || return 1
	done
}

kill_readers() {
	for i in 1 2 3; do
		kill -9 "${RPID[$i]}" 2>/dev/null
		wait "${RPID[$i]}" 2>/dev/null
	done
}

drive() { "$E2E" drive -router "http://127.0.0.1:$ROPORT" -arena "$ARENA" "$@"; }

# router_ready waits up to WAIT_UP_S (30) s for the router to list a manifest, so a first call is not refused as stale.
router_ready() {
	local n=0 out
	while [ $n -lt $((${WAIT_UP_S:-30} * 5)) ]; do
		out=$(drive -rung partial -tool get_ingest_status -limit 1 2>/dev/null)
		[ "$(field "$out" total)" = 1 ] && [ "$(field "$out" s503)" = 0 ] && return 0
		sleep 0.2
		n=$((n + 1))
	done
	return 1
}

ratio() { echo "${1:-?}/${2:-?}"; }

