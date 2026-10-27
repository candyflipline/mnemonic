#!/bin/sh
# p4c-ab.sh BIN:TAG... — lane P4C's timed A/B on /var/lib/mnu/p4-store-b: the
# p4b-drive.sh alternating pairs (each run p4-cycle.sh under flock -x, the
# prod-shaped hot set of laghome-p4b-hot) with P4C's out dir and ports.
# Reported wall numbers: run only after the foreman releases exclusive runs;
# hot-settle.sh resets the store between runs as the drive does not — the
# drive's runs share one warmed store, prime one run first on a cold store.
set -eu
DIR=${DIR:-/var/lib/mnu/p4-store-b}
export OUT=${OUT:-/var/lib/mnu/out/p4c}
export PORT=${PORT:-7871} PUSH_PORT=${PUSH_PORT:-7872}
HERE=$(cd "$(dirname "$0")" && pwd)
exec sh "$HERE/p4b-drive.sh" "$DIR" "$@"
