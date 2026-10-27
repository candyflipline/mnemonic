#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
	echo "usage: install-server.sh <mnemonic-binary>" >&2
	exit 2
fi
BINARY=$1
if [[ ! -x "$BINARY" ]]; then
	echo "install-server.sh: not an executable: $BINARY" >&2
	exit 2
fi
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if ! id mnemonic >/dev/null 2>&1; then
	useradd --system --home-dir /var/lib/mnemonic --shell /usr/sbin/nologin mnemonic
fi

install -d -o mnemonic -g mnemonic -m 750 /var/lib/mnemonic
install -d -o root -g mnemonic -m 750 /etc/mnemonic
install -d -o root -g root -m 755 /usr/local/bin

install -o root -g root -m 755 "$BINARY" /usr/local/bin/mnemonic
install -o root -g root -m 644 "$SCRIPT_DIR/systemd/mnemonic.service" /etc/systemd/system/mnemonic.service

if [[ ! -f /etc/mnemonic/server.toml ]]; then
	install -o root -g mnemonic -m 640 /dev/null /etc/mnemonic/server.toml
	cat > /etc/mnemonic/server.toml <<EOF
data_dir = "/var/lib/mnemonic"

[quic]
listen = ":8741"

# One block per client machine, from: mnemonic key (run on that machine)
# [[keys]]
# key    = "<public key>"
# author = "chris"
EOF
	echo "install-server.sh: wrote /etc/mnemonic/server.toml; add [[keys]] entries, then restart"
fi

if command -v ufw >/dev/null; then
	rules=$(ufw status)
	if grep -q '^Status: active' <<<"$rules" && ! grep -qE '^8741/udp +ALLOW' <<<"$rules"; then
		ufw allow 8741/udp >/dev/null
	fi
fi

systemctl daemon-reload
systemctl enable --now mnemonic.service
systemctl status mnemonic.service --no-pager || true

if [[ -f /var/lib/mnemonic/server.key ]]; then
	out=$(/usr/local/bin/mnemonic key --file /var/lib/mnemonic/server.key)
	echo "server key: ${out%%$'\n'*}"
fi
