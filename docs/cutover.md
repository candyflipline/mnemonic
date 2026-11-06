# How to run a team server

One mnemonic server keeps the agent transcripts of every connected machine and serves them to agents over MCP. Each machine pushes raw transcripts to the server and runs `mnemonic mcp` as its MCP server. To connect a machine, see [How to connect a machine to a team server](team.md).

## Build the binary

The server and the machines run the same binary. The [README](../README.md#install-mnemonic) describes how to install it. To build it yourself, use the `server` build tag:

```
CGO_ENABLED=0 go build -tags server -o mnemonic ./cmd/mnemonic
```

The same command cross-compiles from a Mac with `GOOS=linux GOARCH=amd64`. You can also build on any Linux box and copy the binary over.

## Install the server

Copy the repository to the server, or at least `ops/` and the binary. Then run:

```
sudo ./ops/install-server.sh ./mnemonic
```

The script creates the `mnemonic` user, `/var/lib/mnemonic`, `/usr/local/bin/mnemonic`, and the systemd unit, and then enables the service. On a first install it also writes `/etc/mnemonic/server.toml`. It needs no hostname, no DNS record, and no certificate. The config it writes is:

```toml
data_dir = "/var/lib/mnemonic"

[quic]
listen = ":8741"
```

`listen` is a UDP address. One endpoint carries both client paths, and the ALPN that a client names picks the path: `mnemonic-push/1` for the push stream, and `mnemonic-http/1` for MCP and OTLP.

The server's ed25519 key is `/var/lib/mnemonic/server.key`, which `serve` creates with mode 0600 at the first boot. `serve` logs `server key <key>` at boot, and `mnemonic key --file /var/lib/mnemonic/server.key` prints the key too. Machines pin that key as `server_key`.

## Open the firewall

Open UDP 8741 to the internet and TCP 22 to your allowlist. Nothing else listens. `ss -ltnp` shows no TCP socket held by `mnemonic serve`. When ufw is active, the install script opens UDP 8741 itself. You edit any cloud firewall yourself.

## Add a key for each machine

Each machine has its own key. On the machine, run:

```
mnemonic key
```

The output is the public key and a `[[keys]]` block. Append the block to `/etc/mnemonic/server.toml`, then restart the server:

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

An author can have several keys, one per machine. A key appears once. The private key stays on its machine. A key that the server does not list fails the handshake.

The old `listen`, `push_listen`, `[tls]`, and `[[tokens]]` settings fail to load, with a message that names `[quic]` or `[[keys]]`.

## Serve links

The server serves read links and invite links only when its config has a `[share]` section. It serves them on a loopback listener, and a Cloudflare Tunnel publishes that listener, so no inbound TCP port opens. The design is in [docs/design/share-links.md](design/share-links.md).

```toml
[share]
listen    = "127.0.0.1:8742"
base_url  = "https://share.example.com"
quic_addr = "203.0.113.10:8741"
```

- `listen` is a loopback address.
- `base_url` is the public origin in front of the listener.
- `quic_addr` is the address that `mnemonic join` writes into the config of a joining machine.

To invite a new author, the inviter's key needs the `admin` scope.

The tunnel is `mnemonic-share` in your Cloudflare account, routed by a CNAME for the `base_url` host. On the server:

- `/usr/local/bin/cloudflared` is pinned to 2026.10.5, with no autoupdate.
- `/etc/cloudflared/config.yml` is `ops/cloudflared/config.yml`.
- `/etc/cloudflared/mnemonic-share.json` holds the tunnel's credentials and is in no repository. If you lose it, run `cloudflared tunnel delete mnemonic-share`, then create and route the tunnel again.
- The `cloudflared-mnemonic` systemd unit is `ops/systemd/cloudflared-mnemonic.service` and runs as user `cloudflared`.

Once a `link`, `join`, or `revoke` record exists, a build older than links refuses the control log at boot. Roll back only to a build that reads those records.

## Keep the server key and roll back

`serve` creates the server key at the first boot and derives it from nothing else. If `/var/lib/mnemonic/server.key` is lost, no machine can reach the server until each one pins the new key with `mnemonic init --server quic://HOST:8741 --server-key NEW_KEY --force`. Keep a copy of the key outside any snapshot you rely on to restore the box.

To deploy, set `MNEMONIC_DEPLOY_HOST` to the ssh target and run `ops/deploy.sh`. The script exits naming the variable when it is unset.

```
MNEMONIC_DEPLOY_HOST=root@your-server ops/deploy.sh
```

`ops/deploy.sh` builds on the server and replaces `/usr/local/bin/mnemonic`. It keeps no previous binary. Before a deploy, copy the running binary to `/root/mnemonic.bin.prev-<sha>`, and copy `server.toml` and `mnemonic.service` to `.prev-<sha>` siblings next to it.

To roll back, copy them back over `/usr/local/bin/mnemonic`, `/etc/mnemonic/server.toml`, and `/etc/systemd/system/mnemonic.service`, then run `systemctl restart mnemonic`. Take a snapshot of the server before a deploy that changes stored formats. A rollback target must read every record kind that the server has written (see [Serve links](#serve-links)).

## Known gap

The server serves OTLP only on the QUIC HTTP endpoint. An OTLP exporter that speaks plain HTTPS cannot reach the server, and no forwarder exists.
