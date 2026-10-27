#!/usr/bin/env bash
# The macOS client half of the cutover. upgrade keeps the running binary as
# <bin>.pre-cutover (once), installs the new one, re-registers launchd through
# `init` (config and token kept) and runs one push; rollback puts the kept
# binary back and re-registers launchd with the plist on disk.
#
# Usage: arena/cutover/mac-client.sh upgrade CLIENT_BIN | rollback
# Env: BIN (default ~/.local/bin/mnemonic), LABEL (default com.mnemonic.push)
set -euo pipefail

BIN=${BIN:-$HOME/.local/bin/mnemonic}
LABEL=${LABEL:-com.mnemonic.push}
KEEP=$BIN.pre-cutover
DOMAIN=gui/$(id -u)

running() { launchctl print "$DOMAIN/$LABEL" 2>/dev/null | grep -m1 -E '^\s+state = ' | tr -d '\t'; }

case "${1:-}" in
upgrade)
	[[ $# -eq 2 && -x "$2" ]] || { echo "usage: mac-client.sh upgrade CLIENT_BIN" >&2; exit 2; }
	[[ -f "$KEEP" ]] || cp -p "$BIN" "$KEEP"
	install -m 755 "$2" "$BIN"
	echo "installed $(shasum -a 256 "$BIN" | cut -c1-12) at $BIN (previous kept as $KEEP, $(shasum -a 256 "$KEEP" | cut -c1-12))"
	"$BIN" init --label "$LABEL"
	sleep 2
	echo "launchd $LABEL: $(running)"
	set +e
	"$BIN" push
	rc=$?
	set -e
	echo "one push: rc $rc"
	exit "$rc"
	;;
rollback)
	[[ -f "$KEEP" ]] || { echo "no $KEEP to roll back to" >&2; exit 1; }
	install -m 755 "$KEEP" "$BIN"
	launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
	launchctl bootstrap "$DOMAIN" "$HOME/Library/LaunchAgents/$LABEL.plist"
	sleep 2
	echo "restored $(shasum -a 256 "$BIN" | cut -c1-12) at $BIN; launchd $LABEL: $(running)"
	;;
*)
	sed -n 2,8p "$0" >&2
	exit 2
	;;
esac
