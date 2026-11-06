# How to connect a machine to a team server

A team server keeps the agent transcripts of every connected machine and lets your agents search all of them over MCP. This page takes one machine from nothing to a working connection. To run the server itself, see [How to run a team server](cutover.md). To try mnemonic on one machine without a server, see the [README](../README.md).

A machine reaches the server over one QUIC UDP port, 8741, and proves itself with an ed25519 key. Your network must pass UDP to that port. From the operator you need the server's address, written `HOST` below, and its public key, written `SERVER_KEY`. The server prints that key at boot, and `mnemonic key --file /var/lib/mnemonic/server.key` prints it on the server. The operator needs this machine's key block from you, which step 1 below produces.

## Install mnemonic

Install the binary as the [README](../README.md#install-mnemonic) describes. The same binary runs `mnemonic local` and connects to a team server. It runs on macOS and Linux.

Keep the binary at one stable path, such as `~/.local/bin/mnemonic`. The push agent that `init` installs runs the binary that ran `init`, at its path with symlinks resolved.

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

	`init` writes `~/.mnemonic/config.toml` and installs the push agent. The agent uploads new transcript lines within about 5 seconds of a write.

	- **macOS.** `init` writes `~/Library/LaunchAgents/com.mnemonic.push.plist`, then runs `launchctl bootout` and `launchctl bootstrap` for it and prints the result. A "not loaded" message from bootout is normal on the first install.
	- **Linux.** `init` prints a systemd user unit and the commands that start it. Save the unit as `~/.config/systemd/user/mnemonic-push.service`, then run `systemctl --user daemon-reload` and `systemctl --user enable --now mnemonic-push.service`.

3. Check that the push works. `mnemonic push` runs one pass and prints counts. On macOS the agent logs to `~/.mnemonic/push.log`. On Linux, run `journalctl --user -u mnemonic-push`.

`--machine NAME` sets the machine name. The default is the short hostname. To change an existing config, edit `~/.mnemonic/config.toml` or run `init --force` with both flags again. The file looks like this:

```
server     = "quic://HOST:8741"
server_key = "SERVER_KEY"
machine    = "chris-mbp"
```

`key_file` defaults to `~/.mnemonic/key`. Leave out `[[sources]]` to use the built-in list of transcript directories, the same list `mnemonic local` reads.

If the handshake fails, either the server has no `[[keys]]` block for this machine's key, or `server_key` is not the key the server holds. The error names the keys involved.

## Upgrade the binary

Replace the binary at the same path, then run `init` with no flags. `init` keeps your config and key and registers the push agent again:

```
~/.local/bin/mnemonic init
```

On macOS, `init` boots the agent out and bootstraps it again. Do not use `launchctl kickstart` alone after the binary changed. macOS can refuse to start the agent (`OS_REASON_CODESIGNING`). On Linux, run `systemctl --user restart mnemonic-push.service` instead.

## Point your agents at the server

The MCP server is named `mnemonic`, and its tools appear as `mcp__mnemonic__*`. Every client runs the same command, `mnemonic mcp`. `mnemonic mcp` is a stdio server that reads `~/.mnemonic/config.toml`, or the file that `--config PATH` names, and dials the server over QUIC with this machine's key. No token or environment variable is involved. The client must find `mnemonic` on its `PATH`.

A client marked verified was run with the exact settings shown. A client marked unverified was not run.

### Claude Code

Verified on the spike build on 2026-10-26. `claude -p --strict-mcp-config --mcp-config` with this object connected the server, and `list_sessions` returned a result. To add it for all projects, run:

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

To check it, run `codex exec "Call mnemonic list_sessions for today, limit 1"` and tell the operator whether it returned a result.

### Cursor

Unverified. Put this in `~/.cursor/mcp.json` for all projects, or in `.cursor/mcp.json` for one project:

```
{
	"mcpServers": {
		"mnemonic": { "command": "mnemonic", "args": ["mcp"] }
	}
}
```

To check it, call `list_sessions` once and tell the operator whether it worked.

## Check the connection

Ask your agent: "Call `list_sessions` for today, limit 1." A result that starts with `mnemonic.mcp.list_sessions.v1` means the key, the address, and the config are right. A session you ran a minute ago appears after the next push cycle.

The tools are `search_sessions`, `open`, `list_sessions`, `get_trace`, `file_attention`, `get_ingest_status`, `query`, `annotate`, `set_visibility`, `delete_session`, and `share_link`. `annotate` needs the `annotate` scope on your key. `set_visibility`, `delete_session`, and `share_link` act on your own sessions with any key, and need the `admin` scope for another author's sessions.

## Share sessions with a link

A link is one URL that grants one thing. The server serves links only when the operator has set them up ([Serve links](cutover.md#serve-links)). The design is in [docs/design/share-links.md](design/share-links.md).

### Send a read link

Anyone who holds a read link reads a set of your sessions as they were when you made the link, in a browser or with an agent. They install nothing. Ask your agent to call `share_link`:

```
share_link {"session_ids": ["session:…", "session:…"], "title": "QUIC context", "days": 30}
```

From a terminal, `mnemonic share` takes session ids or Claude Code session uuids and prints the URL. On macOS it also copies the URL:

```
mnemonic share 7c666c01-d38e-4658-8650-854ffb5b626e session:… --title "QUIC context"
```

The answer lists the harness, project, start, message count, and rendered size of each session. Open the link and check it before you send it. `<url>.md` is the whole link as markdown. `<url>/<n>.md` and `<url>/<n>.json` are session `n` alone. Only the sole author of a session, or an admin key, can link it.

### Invite a machine

An invite link admits one machine into your tenant. The machine pushes as the author the invite names and reads what your tenant reads. To invite a machine for your own author, any key works. To invite one for another author, you need an admin key:

```
share_link {"invite": "vlad", "days": 7}
```

On the new machine, install mnemonic and run `mnemonic join <url>`. `join` creates the key, writes `~/.mnemonic/config.toml`, installs the push agent as `init` does, and prints the MCP lines above. Nobody edits `[[keys]]` or copies the server key.

### List and revoke links

`share_link {}` lists your live links, and for each invite the key and machine that joined. `share_link {"revoke": "<link id>"}` ends a link of either kind. A revoked read link answers 410. A machine that joined through a revoked invite is refused at its next request.
