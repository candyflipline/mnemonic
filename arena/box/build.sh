#!/bin/sh
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

for t in oracle gen check seed; do
	{
		echo '#!'/bin/sh
		echo 'export GOMAXPROCS=16'
		echo "exec \"$BIN/$t\" \"\$@\""
	} > "$BIN/wrapper-$t"
	chmod +x "$BIN/wrapper-$t"
done

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
