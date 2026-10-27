#!/bin/sh
# Runs inside its own network namespace (box-run.sh creates it), where 443 and
# 8741 are free and nothing on the host's ports is touched. Boots the serve,
# floods the HTTP listener then the push listener at about 16/s for 3 minutes
# each, waits out the minute ticker so the flood tail lands in summaries,
# collects counts, and stops the serve by its exact pid.
# Usage: in-ns.sh BIN TAG RUN RESULTS
set -u
BIN=$1
TAG=$2
RUN=$3
RESULTS=$4
LOG=$RUN/serve-$TAG.log

export PATH=/usr/sbin:/usr/bin:/sbin:/bin:$PATH
ip link set lo up

"$BIN" serve --config "$RUN/server.toml" >>"$LOG" 2>&1 &
SPID=$!
echo "$SPID" >"$RUN/serve-$TAG.pid"
trap 'kill "$SPID" 2>/dev/null' EXIT

i=0
until grep -q "push serving" "$LOG" 2>/dev/null; do
	i=$((i+1))
	if [ "$i" -gt 100 ]; then
		echo "$TAG FAIL: boot timeout" >>"$RESULTS"
		exit 1
	fi
	kill -0 "$SPID" 2>/dev/null || {
		echo "$TAG FAIL: serve died in boot" >>"$RESULTS"
		exit 1
	}
	sleep 3
done
echo "$TAG boot ok (pid $SPID)" >>"$RESULTS"

sh "$RUN/flood.sh" 127.0.0.1 443 180 16 >"$RUN/flood-$TAG-http.json"
sh "$RUN/flood.sh" 127.0.0.1 8741 180 16 >"$RUN/flood-$TAG-push.json"
sleep 70

kill "$SPID"
wait "$SPID" 2>/dev/null
trap - EXIT

{
	echo "$TAG flood http: $(cat "$RUN/flood-$TAG-http.json")"
	echo "$TAG flood push: $(cat "$RUN/flood-$TAG-push.json")"
	echo "$TAG raw handshake lines: $(grep -c 'http: TLS handshake error from' "$LOG")"
	echo "$TAG summary lines:"
	grep 'TLS handshake errors in the last minute from' "$LOG" | sed 's/^/  /'
	echo "$TAG raw lines per minute:"
	grep 'http: TLS handshake error from' "$LOG" | cut -c1-16 | uniq -c | sed 's/^/  /'
	echo "$TAG grpc/transport lines: $(grep -Eci 'grpc:|transport:' "$LOG" || true)"
	echo "$TAG non-handshake log lines:"
	grep -v 'http: TLS handshake error from' "$LOG" | sed 's/^/  /'
} >>"$RESULTS"
echo "$TAG DONE"
