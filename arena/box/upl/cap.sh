#!/bin/sh
# cap.sh BIN CAPDRV DIR [CAP] — the live-session cap on a fresh store.
#
# One `BIN serve` on an empty DIR/data under MemoryMax=8G in its own scope,
# then CAPDRV opens CAP+1 distinct machines under one push token: exactly one
# must get ResourceExhausted, the other CAP must be acked, and a takeover of
# an admitted machine at the cap must be acked with the displaced stream ended
# Aborted. Then SIGTERM, and the wall from the signal to exit is printed.
# Synthetic input only; counts and codes. Run under the box lock:
#   flock /var/lib/mnu/box.lock sh cap.sh BIN CAPDRV DIR
BIN=$1 CAPDRV=$2 DIR=$3 CAP=${4:-20}
HTTP_PORT=${HTTP_PORT:-7791}
PUSH_PORT=${PUSH_PORT:-7792}
SERVE_PID=
cleanup() { [ -n "$SERVE_PID" ] && kill -TERM "$SERVE_PID" 2>/dev/null; }
trap cleanup EXIT
mkdir -p "$DIR/data"
"$BIN" token new --author cap > "$DIR/token.out"
TOKEN=$(head -1 "$DIR/token.out")
{
  printf 'data_dir = "%s/data"\nlisten = "127.0.0.1:%s"\npush_listen = "127.0.0.1:%s"\n' "$DIR" "$HTTP_PORT" "$PUSH_PORT"
  tail -n +2 "$DIR/token.out"
} > "$DIR/serve.toml"
systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 env GOMAXPROCS=4 "$BIN" serve --config "$DIR/serve.toml" > "$DIR/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 120); do
  kill -0 "$SERVE_PID" 2>/dev/null || { echo 'server died at start' >&2; exit 1; }
  grep -q 'push serving' "$DIR/serve.log" && break
  sleep 0.25
done
grep -q 'push serving' "$DIR/serve.log" || { echo 'push never served' >&2; exit 1; }
"$CAPDRV" -server "127.0.0.1:$PUSH_PORT" -token "$TOKEN" -cap "$CAP" > "$DIR/capdrv.out" 2>&1
rc=$?
cat "$DIR/capdrv.out"
t0=$(date +%s.%N)
kill -TERM "$SERVE_PID"
while kill -0 "$SERVE_PID" 2>/dev/null; do sleep 0.05; done
SERVE_PID=
t1=$(date +%s.%N)
echo "sigterm_to_exit_s $(echo "$t1 - $t0" | bc)"
grep -c 'cycle ' "$DIR/serve.log" | sed 's/^/cycle_lines /'
echo "capdrv_rc $rc"
exit "$rc"
