# mnemonic

mnemonic indexes the session transcripts your coding agents write (Claude Code, Codex, Cursor, and others) and lets an agent search them over MCP. In this guide you build mnemonic, index the sessions already on your machine, and connect Claude Code so it can search them. You need no server and no account.

## Install mnemonic

<!-- INSTALL: pass 2 -->

To build from source, you need Go 1.26.6 or later and read access to this repository:

```
git clone https://github.com/candyflipline/mnemonic
cd mnemonic
CGO_ENABLED=0 go build -tags server -o mnemonic ./cmd/mnemonic
mkdir -p ~/.local/bin
install -m 755 mnemonic ~/.local/bin/mnemonic
```

Put `~/.local/bin` on your `PATH`. Then `command -v mnemonic` prints the path of the binary.

## Index your sessions

Start mnemonic in its own terminal:

```
mnemonic local
```

It indexes the transcripts it finds, then serves MCP at `http://127.0.0.1:7781/mcp`. It prints two lines like these:

```
mnemonic local: 2026/11/05 21:42:41 index: unchanged=0 resumed=0 fresh=1 reparsed=0 in 66ms err=<nil>
mnemonic local: 2026/11/05 21:42:41 listening on 127.0.0.1:7781 (data_dir /Users/you/.mnemonic-local, author you, machine your-mac)
```

`fresh` is the number of transcript files indexed for the first time. Leave `mnemonic local` running. It stays in the foreground until you press Ctrl-C.

If `fresh` is 0, mnemonic found no transcripts. It reads these directories:

- Claude Code: `~/.claude/projects` and `~/.claude-glm/projects`
- Claude desktop agent sessions: `~/Library/Application Support/Claude/local-agent-mode-sessions`
- Codex: `~/.codex/sessions`
- Cursor: `~/.cursor/projects/*/agent-transcripts`
- Kiro CLI: `~/.kiro/sessions/cli`
- Kimi CLI: `~/.kimi/sessions`
- Qwen Code: `~/.qwen/projects/*/chats`
- Pi: `~/.pi/agent/sessions` and `~/.omp/agent/sessions`
- Prime agent: `~/.prime/agent/sessions` and `~/.prime/agent/session-artifacts`

While it runs, mnemonic indexes new writes to these directories within a few seconds. If one of the directories did not exist when `mnemonic local` started, restart `mnemonic local` to index it.

Before it indexes a line, mnemonic removes secrets that its built-in redaction rules match. The index lives in `~/.mnemonic-local`. After a restart, mnemonic reads only the files that changed.

## Connect Claude Code

In a second terminal, add mnemonic for all your projects and check the connection:

```
claude mcp add --transport http --scope user mnemonic http://127.0.0.1:7781/mcp
claude mcp get mnemonic
```

`claude mcp get` prints `Status: ✔ Connected`. Start Claude Code and ask it to search your past sessions for something you worked on. The tools appear as `mcp__mnemonic__search_sessions`, `mcp__mnemonic__open`, `mcp__mnemonic__list_sessions`, and others.

The endpoint has no key or token. Any process on this machine can read your sessions through it. mnemonic refuses a request whose `Origin` header names anything other than a loopback IP address, so a web page in your browser cannot read them. `mnemonic local` listens only on a loopback address.

## Connect Codex or Cursor

To add mnemonic to Codex, run:

```
codex mcp add mnemonic --url http://127.0.0.1:7781/mcp
```

The command writes `url = "http://127.0.0.1:7781/mcp"` under `[mcp_servers.mnemonic]` in `~/.codex/config.toml`, and `codex mcp list` shows the server as `enabled`. A Codex session that calls the tools is untested.

For Cursor, put this in `~/.cursor/mcp.json`. This setting follows Cursor's MCP documentation and is untested:

```
{
	"mcpServers": {
		"mnemonic": { "url": "http://127.0.0.1:7781/mcp" }
	}
}
```

## Next steps

- [Connect a machine to a team server](docs/team.md) to search the sessions of every machine and every teammate in one place.
- [Run a team server](docs/cutover.md).
- [Build a release](docs/release.md).
- [How mnemonic is distributed](docs/distribution.md).

mnemonic is licensed under Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
