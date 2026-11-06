# Release build

Reference for building, naming, versioning and checking a mnemonic release.

## Artifact

A release ships one binary per platform. Each binary is built with `-tags server` and `CGO_ENABLED=0`, so it holds every command, including `mnemonic local`, which exists only in the `server` build (`cmd/mnemonic/local.go`). The build without the tag stays a CI gate and is not shipped.

| Platform | Archive |
|---|---|
| macOS, Apple silicon | `mnemonic_<version>_darwin_arm64.tar.gz` |
| macOS, Intel | `mnemonic_<version>_darwin_amd64.tar.gz` |
| Linux, arm64 | `mnemonic_<version>_linux_arm64.tar.gz` |
| Linux, amd64 | `mnemonic_<version>_linux_amd64.tar.gz` |

Each archive holds `mnemonic`, `LICENSE`, `NOTICE` and `README.md`. `checksums.txt` lists the SHA-256 of every archive and works with `shasum -a 256 -c checksums.txt`. The Linux binaries are statically linked. The binaries are not code-signed or notarized.

`<version>` is the tag without its leading `v`.

## Build recipe

`.goreleaser.yaml` is the whole recipe. GoReleaser is pinned to v2.19.0 and runs through `go run`, so nothing is installed and `go.mod` does not list it.

```sh
go run github.com/goreleaser/goreleaser/v2@v2.19.0 check
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean
```

`check` validates the config. `release --snapshot --clean` builds the four archives and `checksums.txt` into `dist/` (git-ignored) and publishes nothing. The first run downloads GoReleaser's module graph; later runs reuse the Go build cache.

A snapshot is named from the latest tag reachable from `HEAD`: `{{ incpatch .Version }}-snapshot-<short commit>`. While the newest tag is the Rust seed's `v0.7.3`, a snapshot reads `0.7.4-snapshot-<short commit>`.

## Version

The linker stamps `main.version` and `main.commit` (`cmd/mnemonic/version.go`), and stamps `internal/mcp.Version`, which the MCP `initialize` answer reports as `serverInfo.version`. `mnemonic version` prints the first two on one line.

```
mnemonic 0.7.4-snapshot-3553b0af (3553b0af)
```

What a binary prints depends on how it was built.

| Build | Prints |
|---|---|
| GoReleaser | `mnemonic <version> (<short commit>)`, from the flags |
| `go install -tags server github.com/candyflipline/mnemonic/cmd/mnemonic@v0.8.0-rc.1` | `mnemonic 0.8.0-rc.1 (none)`. The module version replaces `dev`, and no revision is recorded. |
| `go build` in a git checkout | `mnemonic dev (<7-character revision>)`. Go also records a pseudo-version here, and the binary ignores it. In a linked `git worktree`, Go records no revision and the binary prints `mnemonic dev (none)`. |

The binary uses the module version only when the build records no `vcs.revision`. A binary that records neither prints `dev`. `mnemonic` with no arguments prints every registered command, sorted, built from the `commands` map in `cmd/mnemonic/main.go`.

The 32 tags `v0.1.0` through `v0.7.3` belong to the Rust seed (branch `archive/rust-seed`). The first Go release proposed is `v0.8.0-rc.1`, which sorts above them. GoReleaser marks a tag with a prerelease suffix as a prerelease.

## Install from a release

Three ways install a release. All of them need the tag to exist and the repository to be public.

### Installer

`install.sh` at the repository root is POSIX `sh` with `set -eu`. It needs `curl`, `tar`, and `sha256sum` or `shasum`, plus the standard POSIX tools, and it never uses `sudo`.

```sh
curl -fsSL https://raw.githubusercontent.com/candyflipline/mnemonic/main/install.sh | sh
```

The script does these steps in order, and stops with a message that names the cause at the first failure.

1. Maps `uname -s` to `darwin` or `linux` and `uname -m` to `amd64` or `arm64`. `x86_64` and `amd64` give `amd64`. `arm64` and `aarch64` give `arm64`. Any other value stops the script and names what it saw.
2. Picks the tag. `MNEMONIC_VERSION` wins. Otherwise the script asks `https://api.github.com/repos/candyflipline/mnemonic/releases?per_page=1` and takes the first `tag_name`.
3. Downloads `mnemonic_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from `<base>/<tag>/`.
4. Compares the SHA-256 of the archive with its line in `checksums.txt`. A mismatch, or a missing line, installs nothing.
5. Extracts `mnemonic`, copies it to `<dir>/.mnemonic.<pid>`, sets mode 755, and renames it over `<dir>/mnemonic`. The rename leaves a running `mnemonic local` untouched.
6. Prints the path and `mnemonic version`, then a `PATH` hint if `<dir>` is not on `PATH`, then `mnemonic local` as the next command.

| Variable | Default | Effect |
|---|---|---|
| `MNEMONIC_VERSION` | newest release, prereleases included | The tag to install, with its `v`, such as `v0.8.0-rc.1`. |
| `MNEMONIC_INSTALL_DIR` | `$HOME/.local/bin` | The directory that receives `mnemonic`. The script creates it. |
| `MNEMONIC_BASE_URL` | `https://github.com/candyflipline/mnemonic/releases/download` | The URL that holds one directory per tag. Point it at a local file server to test the script against `dist/`. |

The script resolves the tag itself because `releases/latest/download/...` skips prereleases. The REST documentation does not state the order of the releases list. In the lists of two public repositories, the first entry was the newest by `created_at`, prereleases included, and the script relies on that.

### Archive

Download `mnemonic_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from `https://github.com/candyflipline/mnemonic/releases/download/<tag>/`, verify with `shasum -a 256 --ignore-missing -c checksums.txt`, extract, and copy `mnemonic` onto `PATH`. `<tag>` carries the `v` and `<version>` does not.

### go install

`go install -tags server github.com/candyflipline/mnemonic/cmd/mnemonic@<tag>` builds the binary on the user's machine. It needs Go 1.26.6, the `go` line in `go.mod`. The `-tags server` flag is required, because `mnemonic local` exists only in that build.

Go's `@latest` selects the highest release version, then the highest prerelease, then a pseudo-version of the default branch's tip. A release version beats a higher prerelease. Origin holds no tags today, so `@latest` resolves to a pseudo-version of `main`. After `v0.8.0-rc.1` is pushed it resolves to that tag, unless `v0.7.3` is also on origin, in which case `@latest` resolves to `v0.7.3`. Install by tag in the docs and in support answers.

The 32 Rust-seed tags `v0.1.0` through `v0.7.3` exist only in local clones. `git push --tags` from such a clone publishes them. Push the release tag by name: `git push origin v0.8.0-rc.1`.

## CI

`.github/workflows/release.yml` runs on a push of a tag matching `v*`. It checks out with full history, installs the Go version in `go.mod`, and runs GoReleaser v2.19.0 with `release --clean` under `GITHUB_TOKEN` with `contents: write`. GoReleaser creates the GitHub release for the tag and attaches the four archives and `checksums.txt`.

## Smoke check

The smoke check proves an extracted archive answers one MCP call from a fixture, with no server and no access to real sessions. Run it on the `darwin_arm64` archive from `dist/`.

1. Create a throwaway home: `export HOME=$(mktemp -d)`, and unset every `XDG_*` variable.
2. Copy `testdata/fixtures/claude-code/session.jsonl` to `$HOME/.claude/projects/demo/session.jsonl`. `mnemonic local` ingests the default sources, all relative to `$HOME`.
3. Extract the archive and run `mnemonic local --listen 127.0.0.1:17781`. Wait for the log line `listening on`.
4. POST to `http://127.0.0.1:17781/mcp` with `Content-Type: application/json`, `Accept: application/json, text/event-stream` and `Mcp-Protocol-Version: 2025-06-18`: an `initialize` request, then a `tools/call` of `search_sessions` with the query `inspect sample project`.
5. The result lists one hit whose `session.id` is `session:` followed by the unpadded base64 of the fixture's `sessionId`, `7c666c01-d38e-4658-8650-854ffb5b626e`.

Never run the steps with the real `HOME`: `mnemonic local` would ingest the real session corpus. Never run `mnemonic init`, `mnemonic join` or `mnemonic push --watch` as part of a smoke check, in any home. They register the `com.mnemonic.push` launchd label in the current user's real GUI domain.

## Limits

- `mnemonic local` serves only while it runs. No resident agent keeps it up.
