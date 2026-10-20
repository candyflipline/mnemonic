# Cutover: moraine → mnemonic

One mnemonic server replaces the ClickHouse host and every local moraine-mcp
process. Clients push raw traces to it and point their MCP config at it.

## Build

Client (any Mac, pure Go, no cgo):

```
CGO_ENABLED=0 go build -o mnemonic ./cmd/mnemonic
```

Server (needs DuckDB, so it needs cgo):

```
go build -tags server -o mnemonic ./cmd/mnemonic
```

Cross-compiling the server from a Mac does not work: DuckDB needs cgo, and
cgo needs a target toolchain. Build the server on the server itself, or on
any Linux box, and copy the binary over.

## Server install

Copy the repo to the server (or at least `ops/` and the binary), then:

```
sudo ./ops/install-server.sh ./mnemonic
```

The script creates the `mnemonic` user, `/var/lib/mnemonic`,
`/usr/local/bin/mnemonic`, and the systemd unit, then enables the service.

Caddy fronts it. Put `ops/Caddyfile` in `/etc/caddy/` (or include it), set
`MNEMONIC_HOST` to the public hostname, and reload Caddy.

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

Get your token file from the owner. The server keeps one per teammate at
`/root/mnemonic-tokens/<author>.token`. Never paste it in chat or commit it.

Install the client (or grab a release binary):

```
CGO_ENABLED=0 go install github.com/candyflipline/mnemonic/cmd/mnemonic@latest
```

`mnemonic init` writes `~/.mnemonic/config.toml` and `~/.mnemonic/token`
(0600) and, on macOS, `~/Library/LaunchAgents/com.mnemonic.push.plist`,
then prints the one `launchctl bootstrap gui/$UID ...` command to run (on
Linux it prints a systemd --user unit):

```
mnemonic init --server https://loyal-agent-memory.5-78-147-233.sslip.io \
  --token-file ./<author>.token
```

`--machine NAME` overrides the machine name (it defaults to the short
hostname); `--force` overwrites an existing config.

Point Claude Code at the server:

```
claude mcp add --transport http moraine \
  https://loyal-agent-memory.5-78-147-233.sslip.io/mcp \
  --header "Authorization: Bearer $(cat ~/.mnemonic/token)"
```

Point Codex at it, in `~/.codex/config.toml`:

```
[mcp_servers.moraine]
url = "https://loyal-agent-memory.5-78-147-233.sslip.io/mcp"
http_headers = { Authorization = "Bearer <token>" }
```

`bearer_token_env_var = "MNEMONIC_TOKEN"` instead keeps the token out of
the file, if you export `MNEMONIC_TOKEN`. The MCP server name stays
`moraine`, so existing agent prompts keep working.

## Client config

`~/.mnemonic/config.toml`:

```
server  = "https://mnemonic.example.com"
token   = "<the token from mnemonic token new>"
machine = "chris-mbp"

[[sources]]
harness     = "claude-code"
source_name = "claude"
glob        = "~/.claude/projects/**/*.jsonl"
```

Omit `[[sources]]` to use the built-in defaults. `token_file` can hold the
token instead of `token`.

## Push on every Mac

`mnemonic push` runs from launchd every 60 s. Install the plist (shipped by
the client lane as `com.mnemonic.push.plist`), then:

```
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.mnemonic.push.plist
```

## Point Claude Code at it

The MCP server name stays `moraine`, so existing `mcp__moraine__*` callers
keep working:

```
claude mcp add --transport http moraine https://mnemonic.example.com/mcp \
  --header "Authorization: Bearer <token>"
```

## Point Codex at it

`~/.codex/config.toml`:

```
[mcp_servers.moraine]
url = "https://mnemonic.example.com/mcp"

[mcp_servers.moraine.http_headers]
Authorization = "Bearer <token>"
```

## Import the ClickHouse archive

On the old ClickHouse host:

```
scripts/export_ch.sh <export_dir>
```

Copy `<export_dir>` to the mnemonic server, then:

```
mnemonic import-ch --config /etc/mnemonic/server.toml --author chris <export_dir>
```

Idempotent: rows whose `source_file` is already in the lake are skipped.

## Stop moraine

After a mnemonic server answers search, open and list_sessions for your
team:

```
launchctl bootout gui/$(id -u)/com.loyal.moraine.ingest
```

Remove the local `moraine` MCP entry from Claude Code (`claude mcp remove
moraine`) and the `[mcp_servers.moraine]` block from Codex config — after
saving a copy for rollback.

Keep the ClickHouse data and the export dir until cutover is proven.

## Rollback

Put the saved moraine MCP entry back (or re-run `claude mcp add` with the old
local command), then re-run moraine ingest:

```
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.loyal.moraine.ingest.plist
```

Traces pushed to mnemonic are raw files under `/var/lib/mnemonic`; nothing is
lost by rolling back.
