#!/usr/bin/env bash
# Ship HEAD to the mnemonic server: copy the source, build there (pure Go, so
# a cross-compiled binary works too), install with install-server.sh, restart.
#
# Usage: ops/deploy.sh [root@host]
set -euo pipefail
HOST=${1:-root@5.78.147.233}
tgz=$(mktemp -t mnemonic-src).tgz
git archive --format=tar.gz -o "$tgz" HEAD
scp -q "$tgz" "$HOST:/root/mnemonic-src.tgz"
rm -f "$tgz"
ssh "$HOST" 'set -e
S=/root/mnemonic-src-$(date +%s)
mkdir -p "$S" && tar -C "$S" -xzf /root/mnemonic-src.tgz && ln -sfn "$S" /root/mnemonic-src
cd "$S" && PATH=/usr/local/go/bin:$PATH GOFLAGS=-p=2 CGO_ENABLED=0 go build -tags server -o /root/mnemonic.bin ./cmd/mnemonic
bash ops/install-server.sh /root/mnemonic.bin >/dev/null
systemctl restart mnemonic
systemctl show mnemonic -p MainPID -p ActiveState'
