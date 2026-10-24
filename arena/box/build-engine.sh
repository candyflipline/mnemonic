#!/bin/sh
# build-engine.sh — build the engine binary on the box from a source tarball.
#
## Inputs
##   $1  TARBALL  git archive tar of the engine tree, made on the Mac with
##                `git archive --format=tar <commit>` and shipped over.
##   $2  TREE     the commit sha that tarball was made from; names the install
##                dir, like build.sh.
## Env: BOX (default /var/lib/mnemonic-e3) — install root.
##      GOFLAGS (default -p=2) — keep the build light while another run holds
##      the box; set GOFLAGS= to build full width.
#
# Builds cmd/mnemonic with CGO_ENABLED=0 and NO build tags: the engine's
# `index` subcommand is in the default build, and at the engine heads so far
# the `server` tag still pulls internal/lake -> go-duckdb, which cgo-rejects
# under CGO_ENABLED=0. Writes $BOX/bin-$TREE/mnemonic, a wrapper-engine that
# execs `mnemonic index "$@"` with GOMAXPROCS=16 (check and the timed scripts
# call `ingest`/`batch` on it, the arena contract), and sha256.txt.
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
TARBALL=${1:?usage: build-engine.sh TARBALL TREE}
TREE=${2:?usage: build-engine.sh TARBALL TREE}
export PATH=/usr/local/go/bin:$PATH
export GOFLAGS=${GOFLAGS:--p=2}

SRC=/root/src/engine-build
rm -rf "$SRC"
mkdir -p "$SRC"
tar -xf "$TARBALL" -C "$SRC"
cd "$SRC"

BIN=$BOX/bin-$TREE
rm -rf "$BIN"
mkdir -p "$BIN"
CGO_ENABLED=0 go build -o "$BIN/mnemonic" ./cmd/mnemonic
{
	echo "source: engine-build of $TREE"
	echo "built: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	go version
	echo "cgo: CGO_ENABLED=0, no build tags (index is tag-free; the server tag still needs cgo for the DuckDB lake at this head)"
	sha256sum "$BIN/mnemonic"
} > "$BIN/sha256.txt"
cat "$BIN/sha256.txt"

{
	echo '#!'/bin/sh
	echo 'export GOMAXPROCS=16'
	echo "exec \"$BIN/mnemonic\" index \"\$@\""
} > "$BIN/wrapper-engine"
chmod +x "$BIN/wrapper-engine"

echo "build-engine.sh: $TREE installed in $BIN"
