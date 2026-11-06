#!/bin/sh
set -eu

repo=candyflipline/mnemonic
base=${MNEMONIC_BASE_URL:-https://github.com/$repo/releases/download}
dir=${MNEMONIC_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

kernel=$(uname -s)
machine=$(uname -m)
case $kernel in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "unsupported OS '$kernel' (uname -s). Releases cover Darwin and Linux." ;;
esac
case $machine in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture '$machine' (uname -m). Releases cover x86_64 and arm64." ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

tag=${MNEMONIC_VERSION:-}
if [ -z "$tag" ]; then
	curl -fsSL -o "$work/releases.json" "https://api.github.com/repos/$repo/releases?per_page=1" ||
		fail "could not ask GitHub for the newest release. Set MNEMONIC_VERSION to a tag such as v0.8.0-rc.1."
	tag=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$work/releases.json" | head -n 1)
	[ -n "$tag" ] || fail "$repo has no published release. Set MNEMONIC_VERSION to a tag."
fi
version=${tag#v}
archive=mnemonic_${version}_${os}_${arch}.tar.gz

echo "Downloading $archive ($tag)"
curl -fsSL -o "$work/$archive" "$base/$tag/$archive" || fail "download failed: $base/$tag/$archive"
curl -fsSL -o "$work/checksums.txt" "$base/$tag/checksums.txt" || fail "download failed: $base/$tag/checksums.txt"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$work/checksums.txt")
[ -n "$want" ] || fail "checksums.txt has no entry for $archive"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$work/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$work/$archive" | awk '{ print $1 }')
else
	fail "neither sha256sum nor shasum is installed"
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $archive: expected $want, got $got. Nothing was installed."

tar -xzf "$work/$archive" -C "$work" mnemonic
mkdir -p "$dir"
cp "$work/mnemonic" "$dir/.mnemonic.$$"
chmod 755 "$dir/.mnemonic.$$"
mv -f "$dir/.mnemonic.$$" "$dir/mnemonic"

echo "Installed $dir/mnemonic"
"$dir/mnemonic" version

case ":$PATH:" in
*":$dir:"*) next=mnemonic ;;
*)
	next=$dir/mnemonic
	echo "$dir is not on your PATH. Add it: export PATH=\"$dir:\$PATH\""
	;;
esac
echo "Next, index your sessions and serve them to your agent: $next local"
