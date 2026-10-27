#!/bin/sh
set -eu
DIR=${1:?usage: storesha.sh DIR}
[ -d "$DIR/raw" ] || { echo "no $DIR/raw" >&2; exit 2; }
cd "$DIR/raw"
find . -type f -print0 | xargs -0 -n 64 -P "${JOBS:-8}" sha256sum | sed 's#  \./#  #' | LC_ALL=C sort -k2
