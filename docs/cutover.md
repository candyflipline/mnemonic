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

Copy the repo to the server (or at least `ops/` and the binary), then:

```
sudo ./ops/install-server.sh ./mnemonic
```

The script creates the `mnemonic` user, `/var/lib/mnemonic`,
`/usr/local/bin/mnemonic`, and the systemd unit, then enables the service.

Caddy fronts it from a Docker container (image `caddy:2.10.2-alpine`) that
reaches HTTP at `host.docker.internal:8740` and native gRPC at
`host.docker.internal:8741`. Mount `ops/Caddyfile` as the
container's Caddyfile, set `MORAINE_HOST` to the public hostname, and reload
Caddy. The push stream (`/mnemonic.push.v1.Push/*`) goes to port 8741 over h2c;
everything else goes over HTTP/1.1.

Set both required listener fields in `/etc/mnemonic/server.toml`:

```toml
listen = "0.0.0.0:8740"
push_listen = "0.0.0.0:8741"
```

There is no default for `push_listen`; a missing field, malformed host:port,
or the same address as `listen` fails startup. Bind 8741 like 8740 and keep
both blocked publicly by the hcloud firewall `loyal-agent-memory`, while
allowing the Docker Caddy host connection. Before promotion, main checks
from outside the server that `nc -vz -w 5 PUBLIC_IP 8741` times out (not a
successful connection), as for 8740, and verifies HTTPS push through Caddy.
The client still dials HTTPS port 443; it needs no config change.

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
