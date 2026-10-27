#!/usr/bin/env bash
# Repro of `mnemonic init`'s bootout/bootstrap race (lane FUINIT).
#
# Upgrading over a running push watcher: init runs `launchctl bootout` and
# `bootstrap` back to back (cmd/mnemonic/init.go); bootout returns before
# launchd has dropped the running agent, so bootstrap fails
# "5: Input/output error" and init exits with the agent booted out and
# nothing running. Run with the fixed binary and every upgrade is rc 0 with
# the agent running 3 s after.
#
# Everything happens under a throwaway label (com.mnemonic.fuinit-test) and a
# throwaway HOME: plist, config and push.log land in a mktemp dir, the agent
# watches only that dir, and a scratch `mnemonic serve` (server tag, 127.0.0.1,
# temp data dir, token minted by the binary's own `token new`) holds the
# stream. com.mnemonic.push, the real ~/.mnemonic and the real
# ~/Library/LaunchAgents are never touched, and no temp path is deleted.
#
# Order quirk (real-corpus guard): launchd gives a gui job the real $HOME, and
# the config the fresh init writes globs `~/.claude/...`, which the agent
# expands against that real home. The watcher scans nothing until its first
# successful dial (internal/push/watch.go: no dial, no reconcile), so the
# scratch serve does not exist yet when the fresh init bootstraps its agent;
# that agent is booted out at once and the config's globs are repointed at the
# temp home BEFORE serve starts. The first install then bootstraps the agent
# with the safe config, and only it is ever running while a server is up.
#
# Usage: arena/fuinit/repro.sh [N] [BIN]
#   N    upgrades to run, default 5
#   BIN  mnemonic binary under test (-tags server build); default: built from
#        this tree into the temp dir
set -euo pipefail

N=${1:-5}
LABEL=com.mnemonic.fuinit-test
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

wait_running() { # polls up to 60 s, prints the state line; rc 1 if never running
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

# Scratch server config: temp data dir, token minted by the binary under test.
# The serve itself starts only after the config repoint below.
"$BIN" token new --author fuinit-repro >"$WORK/token.out"
sed -n '1p' "$WORK/token.out" >"$WORK/token.raw"
{
  echo "data_dir = \"$WORK/data\""
  echo "listen = \"127.0.0.1:$HPORT\""
  echo "push_listen = \"127.0.0.1:$PPORT\""
  sed -n '2,$p' "$WORK/token.out"
} >"$WORK/server.toml"

# Fresh install. No server is listening yet, so the agent this bootstraps can
# dial nothing and reads no file (the watcher reconciles only after a dial).
# The URL carries the push listener's port: for http, push dials the URL's own
# host:port (internal/push/stream.go target).
"$BIN" init --server "http://127.0.0.1:$PPORT" --token-file "$WORK/token.raw" --label "$LABEL"
launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true

# Repoint every source glob at the temp home before anything can dial.
sed -i '' "s|~/|$HOMEDIR/|g" "$CONFIG"
echo "== config globs repointed at $HOMEDIR"

nice -n 15 "$BIN" serve --config "$WORK/server.toml" >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 120); do
  grep -q "push serving" "$WORK/serve.log" && break
  sleep 1
done
grep -q "push serving" "$WORK/serve.log" || { echo "serve never came up"; exit 1; }

# Seed the stream: 100 files x 1000 lines staged outside the watched globs, so
# each upgrade can drop a fresh copy into a fresh directory and the watcher is
# mid-push when bootout lands. A trickle writer keeps events flowing through
# the upgrade itself.
awk 'BEGIN{for(i=0;i<1000;i++) printf "{\"type\":\"user\",\"line\":\"seed %d padding padding padding padding padding\"}\n", i}' >"$STAGE/t.jsonl"
for f in $(seq 1 100); do cp "$STAGE/t.jsonl" "$STAGE/s$f.jsonl"; done

# First install with the safe config (the upgrade path: config kept).
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
  sleep 1.3 # the watcher's quiet window elapses; the flush is pushing
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
