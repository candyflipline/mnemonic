#!/bin/sh
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
