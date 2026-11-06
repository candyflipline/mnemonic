#!/usr/bin/env bash
set -euo pipefail
HOST=${MNEMONIC_DEPLOY_HOST:?set MNEMONIC_DEPLOY_HOST to the ssh target, e.g. root@your-server}
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
