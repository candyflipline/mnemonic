#!/bin/sh
set -eu
DIR=${DIR:-/var/lib/mnu/p4-store-b}
export OUT=${OUT:-/var/lib/mnu/out/p4c}
export PORT=${PORT:-7871} PUSH_PORT=${PUSH_PORT:-7872}
HERE=$(cd "$(dirname "$0")" && pwd)
exec sh "$HERE/p4b-drive.sh" "$DIR" "$@"
