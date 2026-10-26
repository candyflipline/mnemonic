#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
	echo "usage: install-server.sh <mnemonic-binary> [public-hostname]" >&2
	exit 2
fi
BINARY=$1
PUBLIC_HOST=${2:-}
if [[ ! -x "$BINARY" ]]; then
	echo "install-server.sh: not an executable: $BINARY" >&2
	exit 2
fi
if [[ ! -f /etc/mnemonic/server.toml && -z "$PUBLIC_HOST" ]]; then
	echo "install-server.sh: no /etc/mnemonic/server.toml yet; pass the public hostname (its A record points here) as the second argument" >&2
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
listen   = ":443"
push_listen = ":8741"

[tls]
hosts = ["$PUBLIC_HOST"]

# One block per teammate, from: mnemonic token new --author <name>
# [[tokens]]
# author = "chris"
# sha256 = "<64 lowercase hex digits>"
EOF
	echo "install-server.sh: wrote /etc/mnemonic/server.toml; add [[tokens]] entries, then restart"
fi

systemctl daemon-reload
systemctl enable --now mnemonic.service
systemctl status mnemonic.service --no-pager || true
