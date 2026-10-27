#!/bin/sh
set -eu
DIR=${1:?usage: p4b-drive.sh DIR BIN:TAG...}
shift
HERE=$(cd "$(dirname "$0")" && pwd)
export OUT=${OUT:-/var/lib/mnu/out/p4b}
export HOME_KEEP=${HOME_KEEP:-/var/lib/mnu/laghome-p4b-hot}
export HOT_EVERY=${HOT_EVERY:-8}
export HOT_ACTIVE=${HOT_ACTIVE:-5 18}
export HOT_IDLE_SET=${HOT_IDLE_SET:-0 1 2 4 10 20 21 22 23 24 25 26 27 28 29 30 31 32}
export N=${N:-10} PORT=${PORT:-7851} PUSH_PORT=${PUSH_PORT:-7852} FIRST_MAX=${FIRST_MAX:-3600} IDX_MAX=${IDX_MAX:-600}
for pair in "$@"; do
	BIN=${pair%%:*}
	TAG=${pair#*:}
	while [ "$(free -g | awk '/^Mem:/{print $7}')" -lt 10 ]; do sleep 30; done
	date -u +"%FT%TZ $TAG start"
	flock -x /var/lib/mnu/box.lock sh "$HERE/p4-cycle.sh" "$BIN" "$DIR" "$TAG" || echo "$TAG rc $?"
done
date -u +"%FT%TZ DRIVE-DONE"
