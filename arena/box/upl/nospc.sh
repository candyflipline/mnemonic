#!/bin/sh
# GCRASH: a W1 pwrite64 ENOSPC fails one push; the SAME server accepts retry.
# Synthetic input only. Run on Linux, never the Mac or prod. Example:
#   setsid nohup flock /var/lib/mnu/box.lock sh arena/box/upl/nospc.sh \
#     /var/lib/mnu/bin/mnemonic /var/lib/mnu/nospc-$(date +%s) \
#     </dev/null >/var/lib/mnu/out/nospc.log 2>&1 &
# BIN must include the NOSPC fix. DIR must be new; nothing is deleted.
# HTTP_PORT defaults to 7795. For the native-gRPC integration, set PUSH_PORT
# to its port and PUSH_LISTEN_KEY to the server TOML key (e.g. push_listen).
# Without those overrides, this also runs against the lane/upl ServeHTTP base.
# The trace suppresses buffers; all generated inputs are synthetic regardless.
# strace counts injections per thread: stop tracing after the one-source push
# fails, before retry, so no later thread can inject another first-call fault.
set -eu
BIN=${1:?usage: nospc.sh BIN NEW_DIR}
DIR=${2:?usage: nospc.sh BIN NEW_DIR}
HTTP_PORT=${HTTP_PORT:-7795}
PUSH_PORT=${PUSH_PORT:-$HTTP_PORT}
PUSH_LISTEN_KEY=${PUSH_LISTEN_KEY:-}
[ "$(uname -s)" = Linux ] || { echo 'Linux box only' >&2; exit 2; }
[ -x "$BIN" ] || { echo 'BIN is not executable' >&2; exit 2; }
case "$DIR" in /var/lib/mnu/*) ;; *) echo 'DIR must be under /var/lib/mnu' >&2; exit 2;; esac
[ ! -e "$DIR" ] || { echo 'DIR must be new' >&2; exit 2; }
case "$DIR$BIN" in *\"*|*\\*) echo 'quotes/backslashes in paths are unsupported' >&2; exit 2;; esac
if [ -n "$PUSH_LISTEN_KEY" ]; then
 case "$PUSH_LISTEN_KEY" in *[!a-z_]*) echo 'invalid TOML listen key' >&2; exit 2;; esac
fi
umask 077
mkdir -p "$DIR/input" "$DIR/baseline" "$DIR/fault"
SERVE_PID=
TRACE_PID=
stop_trace() {
 if [ -n "$TRACE_PID" ]; then
  kill -TERM "$TRACE_PID" 2>/dev/null || true
  wait "$TRACE_PID" 2>/dev/null || true
  TRACE_PID=
 fi
}
stop_server() {
 if [ -n "$SERVE_PID" ]; then
  kill -TERM "$SERVE_PID" 2>/dev/null || true
  # An exact child PID only; a hung server is killed after bounded shutdown.
  for _ in $(seq 1 60); do kill -0 "$SERVE_PID" 2>/dev/null || break; sleep 0.2; done
  if kill -0 "$SERVE_PID" 2>/dev/null; then kill -KILL "$SERVE_PID" 2>/dev/null || true; fi
  wait "$SERVE_PID" 2>/dev/null || true
  SERVE_PID=
 fi
}
cleanup() { stop_trace; stop_server; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"$BIN" token new --author nospc > "$DIR/token.out"
head -1 "$DIR/token.out" > "$DIR/token"
cat > "$DIR/client.toml" <<CFG
server = "http://127.0.0.1:$PUSH_PORT"
token_file = "$DIR/token"
machine = "nospc"
[[sources]]
harness = "codex"
source_name = "nospc-fixture"
glob = "$DIR/input/*.jsonl"
CFG
seed_input() {
 cat > "$DIR/input/session.jsonl" <<'JSON'
{"timestamp":"2026-10-25T00:00:00Z","type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","timestamp":"2026-10-25T00:00:00Z","cwd":"/nospc-fixture"}}
{"timestamp":"2026-10-25T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"synthetic seed"}}
JSON
}
append_input() {
 cat >> "$DIR/input/session.jsonl" <<'JSON'
{"timestamp":"2026-10-25T00:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"synthetic append"}}
JSON
}
start_server() {
 run=$1
 {
  printf 'data_dir = "%s/%s"\nlisten = "127.0.0.1:%s"\n' "$DIR" "$run" "$HTTP_PORT"
  if [ -n "$PUSH_LISTEN_KEY" ]; then printf '%s = "127.0.0.1:%s"\n' "$PUSH_LISTEN_KEY" "$PUSH_PORT"; fi
  tail -n +2 "$DIR/token.out"
 } > "$DIR/$run.toml"
 GOMAXPROCS=2 "$BIN" serve --config "$DIR/$run.toml" > "$DIR/$run-serve.log" 2>&1 &
 SERVE_PID=$!
 printf '%s\n' "$SERVE_PID" > "$DIR/$run.pid"
 ready=0
 for _ in $(seq 1 120); do
  kill -0 "$SERVE_PID" 2>/dev/null || { echo 'server died at start' >&2; exit 1; }
  if grep -q "listening on" "$DIR/$run-serve.log" && curl -sS --max-time 1 "http://127.0.0.1:$HTTP_PORT/mcp" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.25
 done
 [ "$ready" = 1 ] || { echo 'server readiness timeout' >&2; exit 1; }
}
push_ok() {
 label=$1
 timeout 60 "$BIN" push --config "$DIR/client.toml" > "$DIR/$label-client.log" 2>&1
 grep -q 'scanned 1,' "$DIR/$label-client.log"
 grep -q 'failed 0,' "$DIR/$label-client.log"
}
# Same source path and chunk boundaries in both runs. Only updated_ms is
# masked below, as already established by upl-box/equiv.sh.
seed_input
start_server baseline
push_ok baseline-seed
append_input
push_ok baseline-append
stop_server
seed_input
start_server fault
push_ok fault-seed
RAW=$(find "$DIR/fault/raw" -type f -name '*.jsonl.zst')
[ "$(printf '%s\n' "$RAW" | wc -l)" -eq 1 ] && [ -f "$RAW" ] || { echo 'expected one seeded raw' >&2; exit 1; }
sha256sum "$RAW" | cut -d' ' -f1 > "$DIR/before-raw.sha"
# -P confines injection to this seeded raw, excluding index IO and sockets.
# -e raw prevents strace from decoding the write buffer into the evidence log.
strace -f -p "$SERVE_PID" -P "$RAW" -e trace=pwrite64 -e raw=pwrite64 \
 -e inject=pwrite64:error=ENOSPC:when=1 -o "$DIR/fault-strace.log" \
 2> "$DIR/strace-attach.log" &
TRACE_PID=$!
printf '%s\n' "$TRACE_PID" > "$DIR/strace.pid"
attached=0
for _ in $(seq 1 100); do
 kill -0 "$TRACE_PID" 2>/dev/null || { echo 'strace attach failed' >&2; exit 1; }
 if grep -q 'attached' "$DIR/strace-attach.log"; then attached=1; break; fi
 sleep 0.1
done
[ "$attached" = 1 ] || { echo 'strace attach timeout' >&2; exit 1; }
append_input
rc=0
timeout 60 "$BIN" push --config "$DIR/client.toml" > "$DIR/injected-client.log" 2>&1 || rc=$?
stop_trace
[ "$rc" -ne 0 ] && [ "$rc" -ne 124 ] || { echo 'injected push did not fail as expected' >&2; exit 1; }
grep -q ResourceExhausted "$DIR/injected-client.log"
grep -q 'ENOSPC.*INJECTED' "$DIR/fault-strace.log"
kill -0 "$SERVE_PID"
sha256sum "$RAW" | cut -d' ' -f1 > "$DIR/rolled-back-raw.sha"
cmp "$DIR/before-raw.sha" "$DIR/rolled-back-raw.sha"
push_ok after-fault
kill -0 "$SERVE_PID"
stop_server
# Save per-file sha manifests, then compare them. Manifest timestamps are
# checked for type and normalized; raw frames are hashed without modification.
python3 -I - "$DIR" <<'PY'
import hashlib, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
lists = []
for run in ('baseline', 'fault'):
    files = sorted(p for p in (root / run / 'raw').rglob('*') if p.is_file())
    assert len(files) == 2, ('expected raw and manifest only', run, len(files))
    entries = []
    for p in files:
        data = p.read_bytes()
        assert data and not p.name.startswith('.'), ('empty file or temp', p.name)
        if p.suffix == '.json':
            manifest = json.loads(data)
            assert type(manifest.get('updated_ms')) is int
            manifest['updated_ms'] = 0
            data = json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()
        else:
            assert p.name.endswith('.jsonl.zst')
        entries.append(hashlib.sha256(data).hexdigest() + '  ' + str(p.relative_to(root / run / 'raw')))
    text = '\n'.join(entries) + '\n'
    (root / (run + '-store.sha')).write_text(text)
    lists.append(text)
assert lists[0] == lists[1], 'store sha manifests differ'
print('PASS: injected push ResourceExhausted; same server committed retry; store SHA manifests match (updated_ms masked); no temps')
PY
printf 'date_pt\t%s\nevidence_dir\t%s\ninjected_client_exit\t%s\n' "$(TZ=America/Los_Angeles date '+%Y-%m-%d %H:%M:%S %Z')" "$DIR" "$rc"
