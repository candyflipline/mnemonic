# mnemonic

mnemonic keeps the agent traces (Claude Code, Codex, Cursor, and others) of every machine on one server and lets your agents search them over MCP. This page takes one machine from nothing to a working setup. The server side (install, keys, firewall) is in [docs/cutover.md](docs/cutover.md).

A machine reaches the server over one QUIC UDP port, 8741, and proves itself with an ed25519 key. Your network must pass UDP to that port. From the operator you need the server's address, written `HOST` below, and its public key, written `SERVER_KEY`. The server prints that key at boot, and `mnemonic key --file /var/lib/mnemonic/server.key` prints it on the server. The operator needs this machine's key block from you, which step 1 below produces.

## Install the client

The client is one static binary. It runs on macOS and Linux and needs Go 1.26.6 or later to build.

```
CGO_ENABLED=0 go install github.com/candyflipline/mnemonic/cmd/mnemonic@latest
mkdir -p ~/.local/bin
install -m 755 "$(go env GOPATH)/bin/mnemonic" ~/.local/bin/mnemonic
```

Put `~/.local/bin` on your `PATH`. The scheduler in the next section runs the binary at the path you run `init` from, so use one stable path like this one.

## Connect the machine

1. Print this machine's key. The command creates `~/.mnemonic/key` (mode 0600) on first run:

   ```
   mnemonic key
   ```

   It prints the public key and a ready `[[keys]]` block. Send the block to the operator. The operator adds it to the server config and restarts the server. The private key never leaves the machine.

2. Run `init` with the server address and key:

   ```
   mnemonic init --server quic://HOST:8741 --server-key SERVER_KEY
   ```

   `init` writes `~/.mnemonic/config.toml` and installs the push agent that uploads new trace lines within about 5 seconds of a write.

   - **macOS.** `init` writes `~/Library/LaunchAgents/com.mnemonic.push.plist`, then runs `launchctl bootout` and `launchctl bootstrap` for it and prints the result. A "not loaded" message from bootout is normal on the first install.
   - **Linux.** `init` prints a systemd user unit and the commands that start it. Save the unit as `~/.config/systemd/user/mnemonic-push.service`, then run `systemctl --user daemon-reload` and `systemctl --user enable --now mnemonic-push.service`.

3. Check that the push works. `mnemonic push` runs one pass and prints counts. On macOS the agent logs to `~/.mnemonic/push.log`. On Linux, run `journalctl --user -u mnemonic-push`.

`--machine NAME` overrides the machine name (the short hostname by default). To change an existing config, edit `~/.mnemonic/config.toml` or run `init --force` with both flags again. The file looks like this:

```
server     = "quic://HOST:8741"
server_key = "SERVER_KEY"
machine    = "chris-mbp"
```

`key_file` defaults to `~/.mnemonic/key`. Leave out `[[sources]]` to use the built-in list of harness directories.

If the handshake fails, the server has no `[[keys]]` block for this machine's key, or `server_key` is not the key the server holds. The error names the keys involved.

## Upgrade the client

Replace the binary, then run `init` with no flags. It keeps your config and key and registers the agent again.

```
CGO_ENABLED=0 go install github.com/candyflipline/mnemonic/cmd/mnemonic@latest
install -m 755 "$(go env GOPATH)/bin/mnemonic" ~/.local/bin/mnemonic
~/.local/bin/mnemonic init
```

On macOS this boots the agent out and bootstraps it again, which is required: `launchctl kickstart` alone can leave an agent that macOS refuses to start after its binary changed (`OS_REASON_CODESIGNING`). On Linux, run `systemctl --user restart mnemonic-push.service` instead.

## Point your agents at the server

The MCP server is named `mnemonic` and its tools appear as `mcp__mnemonic__*`. Every client runs the same command, `mnemonic mcp`. It is a stdio server that reads `~/.mnemonic/config.toml` (or the file named by `--config PATH`) and dials the server over QUIC with this machine's key. No token or environment variable is involved. The client must find `mnemonic` on its `PATH`.

A client marked verified was run with the exact settings shown. A client marked unverified was not run.

### Claude Code

Verified on the spike build on 2026-10-26. `claude -p --strict-mcp-config --mcp-config` with this object connected the server, and `list_sessions` returned a result. Add it for all projects:

```
claude mcp add-json --scope user mnemonic '{"type":"stdio","command":"mnemonic","args":["mcp"]}'
```

The same object works under `mcpServers.mnemonic` in `~/.claude.json`.

### Codex

Unverified. Put this in `~/.codex/config.toml`:

```
[mcp_servers.mnemonic]
command = "mnemonic"
args = ["mcp"]
```

Check: run `codex exec "Call mnemonic list_sessions for today, limit 1"` and tell the operator whether it returned a result.

### Cursor

Unverified. Put this in `~/.cursor/mcp.json` (all projects) or `.cursor/mcp.json` (one project):

```
{
  "mcpServers": {
    "mnemonic": { "command": "mnemonic", "args": ["mcp"] }
  }
}
```

Check: call `list_sessions` once and tell the operator whether it worked.

## Check the connection

Ask your agent: "Call `list_sessions` for today, limit 1." A result that starts with `mnemonic.mcp.list_sessions.v1` means the key, the address, and the config are right. A session you ran a minute ago appears after the next push cycle.

The tools are `search_sessions`, `open`, `list_sessions`, `get_trace`, `file_attention`, `get_ingest_status`, `query`, `annotate`, `set_visibility`, `delete_session`, and `share_link`. `annotate` needs the `annotate` scope on your key. `set_visibility`, `delete_session` and `share_link` act on your own sessions with any key and need `admin` for another author's.

## Links

A link is one URL that grants one thing. The design is in [docs/design/share-links.md](docs/design/share-links.md).

**Read link.** Anyone holding it reads a set of your sessions, as they were when you made the link, in a browser or with an agent. They install nothing. Ask your agent to call `share_link`:

```
share_link {"session_ids": ["session:…", "session:…"], "title": "QUIC context", "days": 30}
```

From a terminal, `mnemonic share` takes session ids or Claude Code session uuids, prints the URL and, on macOS, copies it:

```
mnemonic share 7c666c01-d38e-4658-8650-854ffb5b626e session:… --title "QUIC context"
```

The answer lists each session's harness, project, start, messages and rendered size. Open the link and check it before you send it. `<url>.md` is the whole link as markdown, and `<url>/<n>.md` and `<url>/<n>.json` are session `n` alone. Only a session's sole author, or an admin key, may link it.

**Invite link.** It admits one machine into your tenant. The machine pushes as the author the invite names and reads what your tenant reads. Inviting a machine for your own author takes any key, and for another author an admin key:

```
share_link {"invite": "vlad", "days": 7}
```

On the new machine, install the client and run `mnemonic join <url>`. It creates the key, writes `~/.mnemonic/config.toml`, installs the push agent as `init` does, and prints the MCP lines above. Nobody edits `[[keys]]` or copies the server key.

`share_link {}` lists your live links and the key and machine that joined each invite. `share_link {"revoke": "<link id>"}` ends either kind. A revoked read link answers 410, and a revoked invite's machine is refused at its next request.

The server serves links only with a `[share]` section in its config:

```
[share]
listen    = "127.0.0.1:8742"            # loopback only
base_url  = "https://share.example.com" # the public origin in front of it
quic_addr = "HOST:8741"                 # what a joining machine dials
```

## Known gap

OTLP is served only on the QUIC HTTP endpoint, so an OTLP exporter that speaks plain HTTPS cannot reach the server and no forwarder exists.
