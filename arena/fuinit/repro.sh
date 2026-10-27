#!/usr/bin/env bash
set -euo pipefail

N=${1:-5}
LABEL=com.mnemonic.fuinit-test
QUIET_WINDOW_PLUS_MARGIN=1.3
DOMAIN=gui/$(id -u)
REPO=$(cd "$(dirname "$0")/../.." && pwd)

WORK=$(mktemp -d)
BIN=${2:-$WORK/bin/mnemonic}
if [ ! -x "$BIN" ]; then
  mkdir -p "$WORK/bin"
  (cd "$REPO" && go build -tags server -o "$WORK/bin/mnemonic" ./cmd/mnemonic)
fi

HOMEDIR=$WORK/home
CONFIG=$HOMEDIR/.mnemonic/config.toml
WATCH=$HOMEDIR/.claude/projects/repro
STAGE=$WORK/stage
mkdir -p "$HOMEDIR" "$(dirname "$CONFIG")" "$STAGE"

SERVE_PID=

free_port() {
  local p=$1
  while nc -z 127.0.0.1 "$p" 2>/dev/null; do p=$((p + 1)); done
  echo "$p"
}

state() {
  launchctl print "$DOMAIN/$LABEL" 2>/dev/null | grep -m1 -E '^\s+state = ' | tr -d '\t' || true
}

wait_running() {
  local s=
  for _ in $(seq 1 120); do
    s=$(state)
    case "$s" in *"state = running"*) echo "$s"; return 0 ;; esac
    sleep 0.5
  done
  echo "${s:-<no entry>}"
  return 1
}

cleanup() {
  launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true
  if [ -n "$SERVE_PID" ]; then kill "$SERVE_PID" 2>/dev/null || true; fi
  echo "== temp dirs left (no rm -r)"
  du -sh "$WORK" "$HOMEDIR" "$WORK/data" 2>/dev/null || true
  echo "$WORK"
}
trap cleanup EXIT

HPORT=$(free_port 18731)
PPORT=$(free_port 18831)

export HOME=$HOMEDIR # os.UserHomeDir() in init: config and plist land in the temp home

"$BIN" token new --author fuinit-repro >"$WORK/token.out"
sed -n '1p' "$WORK/token.out" >"$WORK/token.raw"
{
  echo "data_dir = \"$WORK/data\""
  echo "listen = \"127.0.0.1:$HPORT\""
  echo "push_listen = \"127.0.0.1:$PPORT\""
  sed -n '2,$p' "$WORK/token.out"
} >"$WORK/server.toml"

"$BIN" init --server "http://127.0.0.1:$PPORT" --token-file "$WORK/token.raw" --label "$LABEL"
launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true

sed -i '' "s|~/|$HOMEDIR/|g" "$CONFIG"
echo "== config globs repointed at $HOMEDIR"

nice -n 15 "$BIN" serve --config "$WORK/server.toml" >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 120); do
  grep -q "push serving" "$WORK/serve.log" && break
  sleep 1
done
grep -q "push serving" "$WORK/serve.log" || { echo "serve never came up"; exit 1; }

awk 'BEGIN{for(i=0;i<1000;i++) printf "{\"type\":\"user\",\"line\":\"seed %d padding padding padding padding padding\"}\n", i}' >"$STAGE/t.jsonl"
for f in $(seq 1 100); do cp "$STAGE/t.jsonl" "$STAGE/s$f.jsonl"; done

OUT=$("$BIN" init --label "$LABEL" 2>&1) && RC=0 || RC=$?
echo "== install: rc=$RC"
printf '%s\n' "$OUT"
S=$(wait_running) || echo "== install: agent never ran ($S)"
echo "== agent after install: $S"

for i in $(seq 1 "$N"); do
  S=$(wait_running) || echo "== upgrade $i: agent not running at start ($S)"
  ITER=$WATCH/iter$i
  mkdir -p "$ITER"
  (while :; do printf '{"tick"}\n' >>"$ITER/live.jsonl"; sleep 0.02; done) &
  TRICKLE=$!
  cp "$STAGE"/s*.jsonl "$ITER"/
  sleep "$QUIET_WINDOW_PLUS_MARGIN"
  OUT=$("$BIN" init --label "$LABEL" 2>&1) && RC=0 || RC=$?
  kill "$TRICKLE" 2>/dev/null || true
  wait "$TRICKLE" 2>/dev/null || true
  echo "== upgrade $i: rc=$RC"
  printf '%s\n' "$OUT"
  sleep 3
  S=$(state)
  echo "== upgrade $i: agent 3 s after: ${S:-<no entry>}"
done

echo "== push.log tail"
tail -n 6 "$HOMEDIR/.mnemonic/push.log" || true
