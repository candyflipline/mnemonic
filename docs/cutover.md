# Setup

One mnemonic server keeps every machine's agent traces and serves them to
agents over MCP. Clients push raw traces to it and point their MCP config
at it.

## Build

The client build is in the [README](../README.md).

The server is pure Go too; it needs the server build tag:

```
CGO_ENABLED=0 go build -tags server -o mnemonic ./cmd/mnemonic
```

The same command cross-compiles from a Mac (`GOOS=linux GOARCH=amd64`), or
build on any Linux box and copy the binary over.

## Server install

Point an A record for the public hostname at the server (not proxied), copy
the repo to the server (or at least `ops/` and the binary), then:

```
sudo ./ops/install-server.sh ./mnemonic mnemonic.example
```

The script creates the `mnemonic` user, `/var/lib/mnemonic`,
`/usr/local/bin/mnemonic`, the systemd unit and, on a first install,
`/etc/mnemonic/server.toml`, then enables the service.

mnemonic terminates TLS itself; there is no proxy. The config the script
writes:

```toml
listen = ":443"
push_listen = ":8741"

[tls]
hosts = ["mnemonic.example"]
```

MCP and HTTP OTLP answer on 443, the push stream on 8741, both with one
Let's Encrypt certificate. It is issued on the first TLS handshake by
tls-alpn-01 on 443 (no port 80) and cached in `/var/lib/mnemonic/autocert`
(0700). The unit grants `CAP_NET_BIND_SERVICE` so the `mnemonic` user can
bind 443. `acme_directory =
"https://acme-staging-v02.api.letsencrypt.org/directory"` under `[tls]` uses
Let's Encrypt staging for a trial host. Without `[tls]` both listeners are
plaintext (boxes and tests). With `[tls]`, `listen` must be on port 443.

Open 443 and 8741 to the internet and 22 to your allowlist. A client whose
`server` is `https://mnemonic.example` pushes to `mnemonic.example:8741`;
`push_address = "host:port"` in its config overrides that.

## Tokens

One token per teammate. Anyone can mint one from the client binary:

```
mnemonic token new --author chris
```

Line 1 is the token. Give the token to that teammate and never store it on
the server. Append the printed `[[tokens]]` block to
`/etc/mnemonic/server.toml`, then:

```
sudo systemctl restart mnemonic
```

## Teammate onboarding

Installing the client, pointing Claude Code, Codex and Cursor at the server, and
upgrading are in the [README](../README.md). A teammate needs the server URL
and their token file from the operator.
