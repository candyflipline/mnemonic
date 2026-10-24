# Setup

One mnemonic server keeps every machine's agent traces and serves them to
agents over MCP. Clients push raw traces to it and point their MCP config
at it.

## Build

Client (any Mac, pure Go, no cgo):

```
CGO_ENABLED=0 go build -o mnemonic ./cmd/mnemonic
```

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
Linux it prints a systemd --user service):

```
mnemonic init --server https://loyal-agent-memory.5-78-147-233.sslip.io \
  --token-file ./<author>.token
```

`--machine NAME` overrides the machine name (it defaults to the short
hostname); `--force` overwrites an existing config.

## Point your agents at it

The MCP server name is `mnemonic`; its tools are `mcp__mnemonic__*`. The
token never goes into a config file.

Claude Code — add an `mcpServers` entry in `~/.claude.json`:

```
"mnemonic": {"type":"http","url":"https://loyal-agent-memory.5-78-147-233.sslip.io/mcp","headersHelper":"printf '{\"Authorization\":\"Bearer %s\"}' \"$(cat ~/.mnemonic/token)\""}
```

Codex — in `~/.codex/config.toml`:

```
[mcp_servers.mnemonic]
url = "https://loyal-agent-memory.5-78-147-233.sslip.io/mcp"
bearer_token_env_var = "MNEMONIC_TOKEN"
```

and in `~/.zshenv`:

```
export MNEMONIC_TOKEN="$(cat ~/.mnemonic/token)"
```

## Client config

`~/.mnemonic/config.toml`:

```
server  = "https://mnemonic.example.com"
machine = "chris-mbp"

[[sources]]
harness     = "claude-code"
source_name = "claude"
glob        = "~/.claude/projects/**/*.jsonl"
```

Omit `[[sources]]` to use the built-in defaults. The token lives in
`~/.mnemonic/token`; `token_file` can point somewhere else if you would
rather not use the default path.

## Push on every Mac

`mnemonic push --watch` stays resident under launchd, wakes on kernel file
events and uploads what changed within 5 s; launchd restarts it if it exits. `mnemonic init` installs the plist; to
bootstrap it by hand:

```
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.mnemonic.push.plist
```
