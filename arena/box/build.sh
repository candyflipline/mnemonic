#!/bin/sh
# build.sh — build the arena oracle binaries on the box from a source tarball.
#
## Inputs
##   $1  TARBALL  git archive tar of the tree to build, made on the Mac with
##                `git archive --format=tar HEAD` and shipped over (scp/ssh).
##   $2  TREE     the commit sha that tarball was made from; names the install
##                dir and the sha256 file, so a later lane reruns this with its
##                own tree and gets its own $BOX/bin-$TREE.
## Env: BOX (default /var/lib/mnemonic-e3) — install root.
#
# Builds oracle and gen with CGO_ENABLED=1 (DuckDB) and check and seed with
# CGO_ENABLED=0, per arena/README.md, into $BOX/bin-$TREE, and writes
# $BOX/bin-$TREE/sha256.txt.
#
# Also, once per box, installs:
#   /usr/bin/time.gnu + /usr/bin/time shim — arena/check runs `/usr/bin/time -l`
#   (arena/check/main.go:180), a BSD flag GNU time on Linux rejects with
#   `invalid option`. The shim keeps the real GNU time at time.gnu and
#   translates only `-l`, printing the max RSS line check's parser reads. No
#   timing semantics change.
#   $BOX/wrapper — `GOMAXPROCS=16; exec "$@"`-style launcher around the
#   binaries just built; check runs it via -bin so every phase sees the same
#   GOMAXPROCS as the timed scripts. Regenerated each build for the new bin dir.
set -eu

BOX=${BOX:-/var/lib/mnemonic-e3}
TARBALL=${1:?usage: build.sh TARBALL TREE}
TREE=${2:?usage: build.sh TARBALL TREE}
export PATH=/usr/local/go/bin:$PATH

SRC=/root/src/box-build
rm -rf "$SRC"
mkdir -p "$SRC"
tar -xf "$TARBALL" -C "$SRC"
cd "$SRC"

BIN=$BOX/bin-$TREE
rm -rf "$BIN"
mkdir -p "$BIN"
CGO_ENABLED=1 go build -o "$BIN/oracle" ./arena/oracle
CGO_ENABLED=1 go build -o "$BIN/gen" ./arena/gen
CGO_ENABLED=0 go build -o "$BIN/check" ./arena/check
CGO_ENABLED=0 go build -o "$BIN/seed" ./arena/seed
{
	echo "source: box-build of $TREE"
	echo "built: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	go version
	echo "cgo: CGO_ENABLED=1 for oracle,gen; CGO_ENABLED=0 for check,seed"
	sha256sum "$BIN/oracle" "$BIN/gen" "$BIN/check" "$BIN/seed"
} > "$BIN/sha256.txt"
cat "$BIN/sha256.txt"

# GOMAXPROCS wrapper for check's -bin (and any phase needing the exact env of
# the timed runs). Points at the tree just built.
for t in oracle gen check seed; do
	{
		echo '#!'/bin/sh
		echo 'export GOMAXPROCS=16'
		echo "exec \"$BIN/$t\" \"\$@\""
	} > "$BIN/wrapper-$t"
	chmod +x "$BIN/wrapper-$t"
done

# The /usr/bin/time -l shim (see header). Idempotent; keeps the real binary.
if [ ! -e /usr/bin/time.gnu ]; then
	cp /usr/bin/time /usr/bin/time.gnu
	cat > /usr/bin/time <<'EOF'
#!/bin/sh
# Installed by arena/box/build.sh: translate the BSD-style `-l` that
# arena/check uses into a GNU-time format printing the line check parses.
# Everything else passes through to the real GNU time untouched.
if [ "$1" = "-l" ]; then
	shift
	exec /usr/bin/time.gnu -f '%M maximum resident set size' "$@"
fi
exec /usr/bin/time.gnu "$@"
EOF
	chmod +x /usr/bin/time
fi

echo "build.sh: $TREE installed in $BIN"
