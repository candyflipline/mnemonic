# mnemonic

mnemonic keeps the agent traces (Claude Code, Codex, Cursor, and others) of every machine on one server and lets your agents search them over MCP. This page gets a teammate from nothing to a working setup on one machine. The server side (install, tokens, gateway) is in [docs/cutover.md](docs/cutover.md).

You need two things from the operator who runs the server: the server URL, written `https://SERVER` below, and a token file for you, written `NAME.token`. Do not paste the token in chat or commit it.

## Install the client

The client is one static binary. It runs on macOS and Linux and needs Go 1.26.6 or later to build.

```
CGO_ENABLED=0 go install github.com/candyflipline/mnemonic/cmd/mnemonic@latest
mkdir -p ~/.local/bin
install -m 755 "$(go env GOPATH)/bin/mnemonic" ~/.local/bin/mnemonic
```

Put `~/.local/bin` on your `PATH`. The scheduler in the next section runs the binary at the path you run `init` from, so use one stable path like this one.

## Connect the machine

1. Save your token file somewhere outside any repository, for example `~/NAME.token`.
2. Run `init` with the server URL and the token file:

   ```
   mnemonic init --server https://SERVER --token-file ~/NAME.token
   ```

   `init` copies the token to `~/.mnemonic/token` (mode 0600), writes `~/.mnemonic/config.toml`, and installs the push agent that uploads new trace lines within about 5 seconds of a write.

   - **macOS.** `init` writes `~/Library/LaunchAgents/com.mnemonic.push.plist`, then runs `launchctl bootout` and `launchctl bootstrap` for it and prints the result. A "not loaded" message from bootout is normal on the first install.
   - **Linux.** `init` prints a systemd user unit and the commands that start it. Save the unit as `~/.config/systemd/user/mnemonic-push.service`, then run `systemctl --user daemon-reload` and `systemctl --user enable --now mnemonic-push.service`.

3. Check that the push works. `mnemonic push` runs one pass and prints counts. On macOS the agent logs to `~/.mnemonic/push.log`. On Linux, run `journalctl --user -u mnemonic-push`.

`--machine NAME` overrides the machine name (default: the short hostname). To change an existing config, edit `~/.mnemonic/config.toml` or run `init --force` with both flags again. The file looks like this:

```
server     = "https://SERVER"
token_file = "~/.mnemonic/token"
machine    = "chris-mbp"
```

Leave out `[[sources]]` to use the built-in list of harness directories.

Push streams to SERVER on port 8741 over TLS, and MCP uses port 443, so both must be reachable from your machine. To send push somewhere else, add `push_address = "host:port"` to `config.toml`.

## Upgrade the client

Replace the binary, then run `init` with no flags. It keeps your config and token and registers the agent again.

```
CGO_ENABLED=0 go install github.com/candyflipline/mnemonic/cmd/mnemonic@latest
install -m 755 "$(go env GOPATH)/bin/mnemonic" ~/.local/bin/mnemonic
~/.local/bin/mnemonic init
```

On macOS this boots the agent out and bootstraps it again, which is required: `launchctl kickstart` alone can leave an agent that macOS refuses to start after its binary changed (`OS_REASON_CODESIGNING`). On Linux, run `systemctl --user restart mnemonic-push.service` instead.

## Point your agents at the server

The MCP server is named `mnemonic` and its tools appear as `mcp__mnemonic__*`. MCP is served at `https://SERVER/mcp` and takes the same token as a bearer token. Each config below reads the token from the environment variable `MNEMONIC_TOKEN`, so the token never sits in a config file. Set it once in your shell profile (`~/.zshenv` or `~/.bashrc`), then start the client from a shell that has it:

```
export MNEMONIC_TOKEN="$(cat ~/.mnemonic/token)"
```

Each client was checked on 2026-10-26 from a Mac by one `list_sessions` call, read-only, with the exact settings shown. A client marked unverified was not run.

### Claude Code

Verified with Claude Code 2.1.302 (`--mcp-config` with this object and `MNEMONIC_TOKEN` set: server connected, `list_sessions` returned a result). Add it for all projects:

```
claude mcp add-json --scope user mnemonic '{"type":"http","url":"https://SERVER/mcp","headers":{"Authorization":"Bearer ${MNEMONIC_TOKEN}"}}'
```

The same object works under `mcpServers.mnemonic` in `~/.claude.json`. The `add-json` wrapper itself was not run, because it writes the config file of the machine that did the checking. Claude Code expands `${MNEMONIC_TOKEN}` when it connects.

### Codex

Verified with codex-cli 0.160.0 (`codex exec` with `-c mcp_servers.mnemonic.url=...` and `-c mcp_servers.mnemonic.bearer_token_env_var=...` and `MNEMONIC_TOKEN` set: `list_sessions` completed). The file below sets the same two keys. Put it in `~/.codex/config.toml`:

```
[mcp_servers.mnemonic]
url = "https://SERVER/mcp"
bearer_token_env_var = "MNEMONIC_TOKEN"
```

### Cursor

Unverified. The Cursor CLI and app are not installed on the Mac that did the checking, so no call ran. The format below comes from Cursor's MCP documentation (cursor.com/docs/context/mcp), which describes `url`, `headers`, and `${env:NAME}` interpolation. Put it in `~/.cursor/mcp.json` (all projects) or `.cursor/mcp.json` (one project):

```
{
  "mcpServers": {
    "mnemonic": {
      "url": "https://SERVER/mcp",
      "headers": { "Authorization": "Bearer ${env:MNEMONIC_TOKEN}" }
    }
  }
}
```

Start Cursor from a shell that has `MNEMONIC_TOKEN`. Call `list_sessions` once and tell the operator whether it worked, so this section can be marked verified.

## Check the connection

Ask your agent: "Call `list_sessions` for today, limit 1." A result that starts with `mnemonic.mcp.list_sessions.v1` means the token, the URL, and the config are right. A `401` from the server means the token is wrong or missing. A session you ran a minute ago appears after the next push cycle.

The tools are `search_sessions`, `open`, `list_sessions`, `get_trace`, `file_attention`, `get_ingest_status`, `query`, `annotate`, `set_visibility`, and `delete_session`. `annotate` needs the `annotate` scope on your token. `set_visibility` and `delete_session` act on your own sessions with any token and need `admin` for another author's.
