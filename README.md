# mnemonic

mnemonic indexes the session transcripts your coding agents write (Claude Code, Codex, Cursor, and others) and lets an agent search them over MCP. In this guide you install mnemonic, index the sessions already on your machine, and connect Claude Code so it can search them. You need no server and no account.

## Install mnemonic

Releases cover macOS and Linux on amd64 and arm64. Pick one install method. On a Mac or a Linux machine with Homebrew, run:

```
brew install candyflipline/tap/mnemonic
```

To keep `mnemonic local` running in the background, run `brew services start mnemonic`.

Without Homebrew, run the installer. It picks the newest release for your OS and architecture, checks the archive against the release's `checksums.txt`, and puts `mnemonic` in `~/.local/bin`:

```
curl -fsSL https://raw.githubusercontent.com/candyflipline/mnemonic/main/install.sh | sh
```

The installer prints the installed version. If `~/.local/bin` is not on your `PATH`, it also prints the line that adds it.

To read the script first, or to avoid piping it to `sh`, download the archive yourself. Set `os` to `darwin` or `linux` and `arch` to `arm64` or `amd64`:

```
tag=v0.8.0
os=darwin
arch=arm64
base=https://github.com/candyflipline/mnemonic/releases/download/$tag
curl -fsSLO "$base/mnemonic_${tag#v}_${os}_${arch}.tar.gz"
curl -fsSLO "$base/checksums.txt"
shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf "mnemonic_${tag#v}_${os}_${arch}.tar.gz" mnemonic
mkdir -p ~/.local/bin
install -m 755 mnemonic ~/.local/bin/mnemonic
```

`shasum` prints `OK` for the archive. On Linux, run `sha256sum --ignore-missing -c checksums.txt` instead. The archives and the installer are not code-signed or notarized. [Build a release](docs/release.md) lists every archive name.

With Go 1.26.6 or later, you can build the same release on your machine. Name the tag, and keep `-tags server`, because `mnemonic local` exists only in that build:

```
go install -tags server github.com/candyflipline/mnemonic/cmd/mnemonic@v0.8.0
```

`go install` puts the binary in `$(go env GOPATH)/bin`.

If an earlier `mnemonic` sits first on your `PATH`, such as `~/.local/bin/mnemonic` from the installer, it hides the Homebrew one. Use one install method, and run `command -v mnemonic` to see which binary runs.

Run `mnemonic version`. It prints `mnemonic 0.8.0`, then the release's short commit in parentheses. A `go install` build prints `(none)` there.

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

The endpoint has no key or token. Any process on this machine can read your sessions through it. `mnemonic local` listens only on a loopback address, so other machines cannot reach it.

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
