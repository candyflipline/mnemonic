#!/bin/sh
# storesha.sh DIR — sha256 of every file under DIR/raw, one line per file:
# "<sha256>  <path relative to DIR/raw>", sorted by path. Includes the
# manifests (*.json) and the frames (*.jsonl.zst) and any stray temp file, so a
# leftover shows up as an extra line. Output on stdout; read only, no lock.
# Env: JOBS (default 8) parallel sha256sum processes.
set -eu
DIR=${1:?usage: storesha.sh DIR}
[ -d "$DIR/raw" ] || { echo "no $DIR/raw" >&2; exit 2; }
cd "$DIR/raw"
find . -type f -print0 | xargs -0 -n 64 -P "${JOBS:-8}" sha256sum | sed 's#  \./#  #' | LC_ALL=C sort -k2
