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

The linker stamps `main.version` and `main.commit` (`cmd/mnemonic/version.go`). `mnemonic version` prints both on one line.

```
mnemonic 0.7.4-snapshot-3553b0af (3553b0af)
```

A binary built without the flags prints `dev` and the `vcs.revision` that `go build` records, or `none` when there is none. `mnemonic` with no arguments prints every registered command, sorted, built from the `commands` map in `cmd/mnemonic/main.go`.

Tags `v0.5.6` through `v0.7.3` belong to the Rust seed (branch `archive/rust-seed`). The first Go release proposed is `v0.8.0-rc.1`, which sorts above them. GoReleaser marks a tag with a prerelease suffix as a prerelease.

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
- `serverInfo.version` in the MCP `initialize` answer is `dev` in every build. It comes from `internal/mcp`, not from the stamped `main.version`.
