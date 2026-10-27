#!/usr/bin/env bash
# Held-watcher proof for init's 25 s wait (lane FUINIT3).
#
# launchd's bootout sends SIGTERM and SIGKILLs the job at ExitTimeOut (default
# 20 s; the plist init writes sets none). A watcher that does not act on
# SIGTERM is therefore gone only after ~20 s. This script makes the throwaway
# agent such a watcher by SIGSTOPping its pid right before the upgrade `init`
# (a stopped process cannot act on SIGTERM; SIGKILL ends it), then times:
#   dead  start of init -> the agent pid is gone (launchd's SIGKILL)
#   gone  start of init -> `launchctl print` of the throwaway label fails
#         (may be unseen: bootstrap follows within one poll of init's wait)
#   init  start of init -> init exits, with its rc and last line
# A 15 s init fails "did not exit within 15s"; a 25 s init returns rc 0 with
# the agent running again under a new pid.
#
# Everything runs under the throwaway label com.mnemonic.fuinit-test and a
# throwaway HOME. LIVE-AGENT GUARD: before the first step and after every
# init the script reads `launchctl print gui/<uid>/com.mnemonic.push` (read
# only) and aborts, rc 3 "live agent changed", unless its program is
# /Users/user/.local/bin/mnemonic and its pid equals the pid at script start.
# init is only ever run through run_init, which carries --label; no temp path
# is deleted.
#
# Usage: arena/fuinit/repro-held.sh BIN [N]   (BIN: -tags server build; N default 1)
set -euo pipefail

BIN=${1:?usage: repro-held.sh BIN [N]}
N=${2:-1}
LABEL=com.mnemonic.fuinit-test
LIVE=com.mnemonic.push
LIVE_PROGRAM=/Users/user/.local/bin/mnemonic
DOMAIN=gui/$(id -u)
[ "$LABEL" = com.mnemonic.fuinit-test ] || exit 4
[ -x "$BIN" ] || { echo "no binary $BIN"; exit 1; }

live_field() { # $1 = program | pid
  launchctl print "$DOMAIN/$LIVE" 2>/dev/null | grep -m1 -E "^[[:space:]]+$1 = " | sed -E "s/^[[:space:]]+$1 = //"
}
LIVE_PID0=$(live_field pid)
guard() {
  local prog pid
  prog=$(live_field program)
  pid=$(live_field pid)
  if [ "$prog" != "$LIVE_PROGRAM" ] || [ "$pid" != "$LIVE_PID0" ]; then
    echo "live agent changed: program=$prog pid=$pid (start pid=$LIVE_PID0)"
    exit 3
  fi
}
guard
echo "== live agent guard ok: $LIVE program=$LIVE_PROGRAM pid=$LIVE_PID0"

WORK=$(mktemp -d)
HOMEDIR=$WORK/home
CONFIG=$HOMEDIR/.mnemonic/config.toml
WATCH=$HOMEDIR/.claude/projects/repro
mkdir -p "$HOMEDIR/.mnemonic" "$WATCH"
export HOME=$HOMEDIR
SERVE_PID=

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
free_port() { local p=$1; while nc -z 127.0.0.1 "$p" 2>/dev/null; do p=$((p + 1)); done; echo "$p"; }
field() { launchctl print "$DOMAIN/$LABEL" 2>/dev/null | grep -m1 -E "^[[:space:]]+$1 = " | sed -E "s/^[[:space:]]+$1 = //" || true; }

cleanup() {
  launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true
  if [ -n "$SERVE_PID" ]; then kill "$SERVE_PID" 2>/dev/null || true; fi
  echo "== temp dirs left (no rm -r): $WORK"
  du -sh "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

run_init() { # label always carried; prints output to $1, sets RC
  local out=$1
  shift
  "$BIN" init "$@" --label "$LABEL" >"$out" 2>&1
}

wait_running() { # up to 60 s; echoes pid; rc 1 if never running
  local s
  for _ in $(seq 1 120); do
    s=$(field state)
    if [ "$s" = running ]; then field pid; return 0; fi
    sleep 0.5
  done
  return 1
}

HPORT=$(free_port 18731)
PPORT=$(free_port 18831)
"$BIN" token new --author fuinit-held >"$WORK/token.out"
sed -n '1p' "$WORK/token.out" >"$WORK/token.raw"
{
  echo "data_dir = \"$WORK/data\""
  echo "listen = \"127.0.0.1:$HPORT\""
  echo "push_listen = \"127.0.0.1:$PPORT\""
  sed -n '2,$p' "$WORK/token.out"
} >"$WORK/server.toml"

# Fresh install while no server exists (the watcher scans nothing before its
# first dial), booted out at once; globs repointed at the temp home before the
# serve starts, so the real corpus is never scanned.
run_init "$WORK/fresh.out" --server "http://127.0.0.1:$PPORT" --token-file "$WORK/token.raw"
guard
launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true
sed -i '' "s|~/|$HOMEDIR/|g" "$CONFIG"
echo "== config globs repointed at $HOMEDIR"

nice -n 15 "$BIN" serve --config "$WORK/server.toml" >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 120); do grep -q "push serving" "$WORK/serve.log" && break; sleep 1; done
grep -q "push serving" "$WORK/serve.log" || { echo "serve never came up"; exit 1; }

awk 'BEGIN{for(i=0;i<50;i++) printf "{\"type\":\"user\",\"line\":\"seed %d\"}\n", i}' >"$WATCH/seed.jsonl"

run_init "$WORK/install.out" # the upgrade path: config kept
guard
cat "$WORK/install.out"
PID=$(wait_running) || { echo "agent never ran"; exit 1; }
for _ in $(seq 1 60); do [ -s "$HOMEDIR/.mnemonic/push.log" ] && break; sleep 1; done
echo "== agent running pid=$PID; first pass: $(tail -n 1 "$HOMEDIR/.mnemonic/push.log" 2>/dev/null || echo none)"

FAILS=0
for i in $(seq 1 "$N"); do
  PID=$(wait_running) || { echo "== run $i: agent not running at start"; FAILS=$((FAILS + 1)); break; }
  CMD=$(ps -o command= -p "$PID" || true)
  case "$CMD" in
    *"push --watch"*"$HOMEDIR/.mnemonic/config.toml"*) ;;
    *) echo "== run $i: pid $PID is not the throwaway watcher ($CMD)"; exit 5 ;;
  esac
  echo "== run $i: launchd exit timeout for $LABEL: $(launchctl print "$DOMAIN/$LABEL" | grep -m1 -E 'exit timeout' | tr -d '\t')"
  kill -STOP "$PID"
  echo "== run $i: SIGSTOP pid=$PID ($LABEL)"
  OUTF=$WORK/init$i.out
  T0=$(now)
  run_init "$OUTF" &
  IP=$!
  TDEAD= TGONE= TINIT=
  while :; do
    T=$(now)
    if [ -z "$TDEAD" ] && ! kill -0 "$PID" 2>/dev/null; then TDEAD=$T; fi
    if [ -z "$TGONE" ] && ! launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then TGONE=$T; fi
    if [ -z "$TINIT" ] && ! kill -0 "$IP" 2>/dev/null; then TINIT=$T; fi
    if [ -n "$TINIT" ] && [ -n "$TDEAD" ]; then break; fi
    if awk -v a="$T0" -v b="$T" 'BEGIN{exit !(b-a>90)}'; then echo "== run $i: 90 s without finishing"; break; fi
    sleep 0.05
  done
  RC=0
  wait "$IP" || RC=$?
  guard
  el() { [ -n "$1" ] && awk -v a="$T0" -v b="$1" 'BEGIN{printf "%.1f s", b-a}' || echo unseen; }
  echo "== run $i: dead=$(el "$TDEAD") gone=$(el "$TGONE") init=$(el "$TINIT") rc=$RC"
  echo "== run $i: init last line: $(tail -n 1 "$OUTF")"
  if [ "$RC" -ne 0 ]; then
    FAILS=$((FAILS + 1))
    echo "== run $i: init failed; throwaway agent state: $(field state || true)"
    continue
  fi
  NEWPID=$(wait_running) && echo "== run $i: agent after: state=running pid=$NEWPID (was $PID)" \
    || { echo "== run $i: agent NOT running after"; FAILS=$((FAILS + 1)); }
done
echo "== done: runs=$N init-failures=$FAILS"
guard
echo "== live agent guard ok at end: pid=$LIVE_PID0"
