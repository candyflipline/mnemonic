# Setup

One mnemonic server keeps every machine's agent traces and serves them to
agents over MCP. Clients push raw traces to it and run `mnemonic mcp` as
their MCP server.

## Build

The client build is in the [README](../README.md).

The server is pure Go too; it needs the server build tag:

```
CGO_ENABLED=0 go build -tags server -o mnemonic ./cmd/mnemonic
```

The same command cross-compiles from a Mac (`GOOS=linux GOARCH=amd64`), or
build on any Linux box and copy the binary over.

## Server install

Copy the repo to the server (or at least `ops/` and the binary), then:

```
sudo ./ops/install-server.sh ./mnemonic
```

The script creates the `mnemonic` user, `/var/lib/mnemonic`,
`/usr/local/bin/mnemonic`, the systemd unit and, on a first install,
`/etc/mnemonic/server.toml`, then enables the service. It needs no hostname,
no DNS record and no certificate. The config it writes:

```toml
data_dir = "/var/lib/mnemonic"

[quic]
listen = ":8741"
```

`listen` is UDP. One endpoint carries both client paths, and the ALPN a
client names picks the path: `mnemonic-push/1` for the push stream and
`mnemonic-http/1` for MCP and OTLP. The server's ed25519 key is
`/var/lib/mnemonic/server.key`, created with mode 0600 at the first boot.
`serve` logs `server key <key>` at boot, and `mnemonic key --file
/var/lib/mnemonic/server.key` prints it too. Clients pin that key as
`server_key`.

Open UDP 8741 to the internet and 22 to your allowlist. Nothing else
listens: `ss -ltnp` shows no TCP socket held by `mnemonic serve`. The
install script opens UDP 8741 itself when ufw is active. A cloud firewall is
yours to edit.

## Keys

One key per client machine. A machine prints its own:

```
mnemonic key
```

The output is the public key and a `[[keys]]` block. Append the block to
`/etc/mnemonic/server.toml`, then:

```
sudo systemctl restart mnemonic
```

```toml
[[keys]]
key    = "<public key from mnemonic key>"
author = "chris"
tenant = "acme"            # optional, default "default"; an author sits in one tenant
scopes = ["read", "push"]  # optional, the default; "annotate" and "admin" opt in
```

An author may have several keys, one per machine. A key appears once.
The private key stays on its machine. A key the server does not list fails
the handshake.

The old `listen`, `push_listen`, `[tls]` and `[[tokens]]` settings fail to
load with a message that names `[quic]` or `[[keys]]`.

## Client onboarding

Installing the client, pointing Claude Code, Codex and Cursor at the server,
and upgrading are in the [README](../README.md). A machine needs the server
address and the server key, and the operator needs its `[[keys]]` block.
