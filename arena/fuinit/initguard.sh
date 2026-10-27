#!/usr/bin/env bash
# Proof for the launchd takeover guard in `mnemonic init` (lane INITGUARD).
#
# Two throwaway HOMEs (A, B) share one throwaway label, as one real user does
# across HOMEs. Expected: B without --force refuses, exits non-zero, names A's
# plist and leaves A's job loaded with the same pid; B with --force replaces
# it; A twice in a row is a normal re-init. The owner's live
# com.mnemonic.push job is printed before and after each step and the script
# aborts if its program or pid changes. Every init passes --label; the agent
# dials http://127.0.0.1:1 (nothing listens), so it reads no file. No temp
# path is deleted.
#
# Usage: arena/fuinit/initguard.sh [BIN]
set -euo pipefail

LABEL=com.mnemonic.initguard-test
DOMAIN=gui/$(id -u)
LIVE=$DOMAIN/com.mnemonic.push
REPO=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
BIN=${1:-$WORK/mnemonic}
[ -x "$BIN" ] || (cd "$REPO" && CGO_ENABLED=0 go build -o "$WORK/mnemonic" ./cmd/mnemonic)
A=$WORK/homeA
B=$WORK/homeB
mkdir -p "$A" "$B"
echo token >"$WORK/token"

live() { launchctl print "$LIVE" | grep -E '^\s+(program|pid) = ' | tr -d '\t' | paste -sd' ' -; }
job() { launchctl print "$DOMAIN/$LABEL" 2>/dev/null | grep -E '^\s+(path|pid) = ' | tr -d '\t' | paste -sd' ' - || true; }
BASE=$(live)
check() {
  local now
  now=$(live)
  [ "$now" = "$BASE" ] || { echo "ABORT: live job changed: $BASE -> $now"; exit 2; }
  echo "live: $now"
}
cleanup() {
  launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true
  check
  echo "== temp dir left (no rm -r): $WORK"
}
trap cleanup EXIT

init() { # home extra-args...
  local h=$1
  shift
  HOME=$h "$BIN" init --server http://127.0.0.1:1 --token-file "$WORK/token" --label "$LABEL" "$@"
}
wait_job() { for _ in $(seq 1 40); do [ -n "$(job)" ] && return 0; sleep 0.25; done; return 1; }

check
echo "== 1. HOME=A fresh"; init "$A"; wait_job; echo "job: $(job)"; check
PID1=$(job)
echo "== 2. HOME=B no --force: must refuse"
if init "$B"; then echo "FAIL: init B succeeded"; exit 1; else echo "rc=$?"; fi
echo "job: $(job)"; [ "$(job)" = "$PID1" ] || { echo "FAIL: A's job changed"; exit 1; }
[ ! -e "$B/Library" ] && [ ! -e "$B/.mnemonic" ] && echo "B untouched"; check
echo "== 3. HOME=B --force: replaces"; init "$B" --force; wait_job; echo "job: $(job)"; check
echo "== 4. HOME=A twice (A now needs --force to take the label back from B)"
init "$A" --force; wait_job; echo "job: $(job)"
HOME=$A "$BIN" init --label "$LABEL"; wait_job; echo "job (second run, plain): $(job)"; check
