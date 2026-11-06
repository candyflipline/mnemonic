# Release build

Reference for building, naming, versioning, installing and publishing a mnemonic release. The page ends with the runbook for release day.

## Artifact

A release ships one binary per platform. Each binary is built with `-tags server` and `CGO_ENABLED=0`, so it holds every command, including `mnemonic local`, which exists only in the `server` build (`cmd/mnemonic/local.go`). The build without the tag stays a CI gate and is not shipped.

| Platform | Archive |
|---|---|
| macOS, Apple silicon | `mnemonic_<version>_darwin_arm64.tar.gz` |
| macOS, Intel | `mnemonic_<version>_darwin_amd64.tar.gz` |
| Linux, arm64 | `mnemonic_<version>_linux_arm64.tar.gz` |
| Linux, amd64 | `mnemonic_<version>_linux_amd64.tar.gz` |

Each archive holds `mnemonic`, `LICENSE`, `NOTICE` and `README.md`. `checksums.txt` lists the SHA-256 of every archive and works with `shasum -a 256 --ignore-missing -c checksums.txt`. The Linux binaries are statically linked. The binaries are not code-signed or notarized.

`<version>` is the tag without its leading `v`. The first Go release is `v0.8.0`, a full release, so `<version>` is `0.8.0`.

GoReleaser also writes the Homebrew formula `Formula/mnemonic.rb` into `candyflipline/homebrew-tap` (see [Homebrew](#homebrew)).

## Build recipe

`.goreleaser.yaml` is the whole recipe. GoReleaser is pinned to v2.19.0 and runs through `go run`, so nothing is installed and `go.mod` does not list it.

```sh
go run github.com/goreleaser/goreleaser/v2@v2.19.0 check
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean
```

`check` validates the config. `release --snapshot --clean` builds the four archives and `checksums.txt` into `dist/` (git-ignored) and publishes nothing. The first run downloads GoReleaser's module graph; later runs reuse the Go build cache.

A snapshot is named from the latest tag reachable from `HEAD`: `{{ incpatch .Version }}-snapshot-<short commit>`. While the newest tag is the Rust seed's `v0.7.3`, a snapshot reads `0.7.4-snapshot-<short commit>`. Once `v0.8.0` is tagged, it reads `0.8.1-snapshot-<short commit>`.

## Version

The linker stamps `main.version` and `main.commit` (`cmd/mnemonic/version.go`), and stamps `internal/mcp.Version`, which the MCP `initialize` answer reports as `serverInfo.version`. `mnemonic version` prints the first two on one line.

```
mnemonic 0.7.4-snapshot-3553b0af (3553b0af)
```

What a binary prints depends on how it was built.

| Build | Prints |
|---|---|
| GoReleaser | `mnemonic <version> (<short commit>)`, from the flags |
| `go install -tags server github.com/candyflipline/mnemonic/cmd/mnemonic@v0.8.0` | `mnemonic 0.8.0 (none)`. The module version replaces `dev`, and no revision is recorded. |
| `go build` in a git checkout | `mnemonic dev (<7-character revision>)`. Go also records a pseudo-version here, and the binary ignores it. In a linked `git worktree`, Go records no revision and the binary prints `mnemonic dev (none)`. |

The binary uses the module version only when the build records no `vcs.revision`. A binary that records neither prints `dev`. `mnemonic` with no arguments prints every registered command, sorted, built from the `commands` map in `cmd/mnemonic/main.go`.

The 32 tags `v0.1.0` through `v0.7.3` belong to the Rust seed (branch `archive/rust-seed`). `v0.8.0` sorts above them. It has no prerelease suffix, so GoReleaser publishes it as a full release, and `release.make_latest: true` in `.goreleaser.yaml` makes it the release that GitHub's `releases/latest` resolves to.

## Install from a release

Four ways install a release. All of them need the tag to exist and the repository to be public.

### Installer

`install.sh` at the repository root is POSIX `sh` with `set -eu`. It needs `curl`, `tar`, and `sha256sum` or `shasum`, plus the standard POSIX tools, and it never uses `sudo`.

```sh
curl -fsSL https://raw.githubusercontent.com/candyflipline/mnemonic/main/install.sh | sh
```

The script does these steps in order, and stops with a message that names the cause at the first failure.

1. Maps `uname -s` to `darwin` or `linux` and `uname -m` to `amd64` or `arm64`. `x86_64` and `amd64` give `amd64`. `arm64` and `aarch64` give `arm64`. Any other value stops the script and names what it saw.
2. Picks the tag. `MNEMONIC_VERSION` wins. Otherwise the script asks GitHub for the latest release, `https://api.github.com/repos/candyflipline/mnemonic/releases/latest`, and takes its `tag_name`.
3. Downloads `mnemonic_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from `<base>/<tag>/`.
4. Compares the SHA-256 of the archive with its line in `checksums.txt`. A mismatch, or a missing line, installs nothing.
5. Extracts `mnemonic`, copies it to `<dir>/.mnemonic.<pid>`, sets mode 755, and renames it over `<dir>/mnemonic`. The rename leaves a running `mnemonic local` untouched.
6. Prints the path and `mnemonic version`, then a `PATH` hint if `<dir>` is not on `PATH`, then `mnemonic local` as the next command.

| Variable | Default | Effect |
|---|---|---|
| `MNEMONIC_VERSION` | the latest release | The tag to install, with its `v`, such as `v0.8.0`. |
| `MNEMONIC_INSTALL_DIR` | `$HOME/.local/bin` | The directory that receives `mnemonic`. The script creates it. |
| `MNEMONIC_BASE_URL` | `https://github.com/candyflipline/mnemonic/releases/download` | The URL that holds one directory per tag. Point it at a local file server to test the script against `dist/`. |

GitHub defines the latest release as the most recent release that is neither a prerelease nor a draft. `v0.8.0` is a full release, so `releases/latest` resolves to it. A later prerelease tag would not move `releases/latest`, and the installer would keep installing the previous full release.

### Homebrew

```sh
brew install candyflipline/tap/mnemonic
```

`brew install` adds the tap `candyflipline/homebrew-tap` on first use. The formula is `Formula/mnemonic.rb`. GoReleaser (`brews` in `.goreleaser.yaml`) renders it and pushes it to the tap on every release, authenticated by an SSH deploy key that sits in the Actions secret `HOMEBREW_TAP_DEPLOY_KEY`.

The formula has two blocks beyond `install`.

- `test do` runs `mnemonic version` and asserts that the output holds the formula's version. Run it with `brew test mnemonic`.
- `service do` runs `mnemonic local` with `keep_alive`, and writes its log to `$(brew --prefix)/var/log/mnemonic.log`. `brew services start mnemonic` keeps `mnemonic local` running in the background and `brew services stop mnemonic` stops it.

`mnemonic init` records `<prefix>/opt/mnemonic/bin/mnemonic`, Homebrew's stable path, so the push agent keeps working after `brew upgrade` removes the versioned Cellar directory.

### Archive

Download `mnemonic_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from `https://github.com/candyflipline/mnemonic/releases/download/<tag>/`, verify with `shasum -a 256 --ignore-missing -c checksums.txt`, extract, and copy `mnemonic` onto `PATH`. `<tag>` carries the `v` and `<version>` does not.

### go install

`go install -tags server github.com/candyflipline/mnemonic/cmd/mnemonic@<tag>` builds the binary on the user's machine. It needs Go 1.26.6, the `go` line in `go.mod`. The `-tags server` flag is required, because `mnemonic local` exists only in that build.

Go's `@latest` selects the highest release version, then the highest prerelease, then a pseudo-version of the default branch's tip. Origin holds no tags today, so `@latest` resolves to a pseudo-version of `main`. After `v0.8.0` is pushed it resolves to `v0.8.0`, which is higher than every Rust-seed tag on origin. Install by tag in the docs and in support answers.

The 32 Rust-seed tags `v0.1.0` through `v0.7.3` exist only in local clones. `git push --tags` from such a clone publishes them. Push the release tag by name: `git push origin v0.8.0`.

## CI

Two workflows build a release.

`.github/workflows/smoke.yml` builds the GoReleaser snapshot once and runs `scripts/smoke.sh` on the `linux_amd64`, `linux_arm64`, `darwin_arm64` and `darwin_amd64` archives, each on a runner of that platform. It runs on every push to `main` and to `lane/**`, and `release.yml` reuses it through `workflow_call`.

`.github/workflows/release.yml` runs on a push of a tag matching `v*`. It runs these jobs in order.

1. The gates from `AGENTS.md`.
2. `smoke.yml`.
3. GoReleaser v2.19.0 with `release --clean`, under `contents: write`. The job needs the first two to pass. It creates the GitHub release for the tag, attaches the four archives and `checksums.txt`, and pushes the formula to the tap with `HOMEBREW_TAP_DEPLOY_KEY`.

A tag whose gates or smoke fail publishes nothing.

## Smoke check

`scripts/smoke.sh <archive>` proves that an extracted archive answers one MCP call from a fixture, with no server and no access to real sessions.

1. It creates a throwaway home and unsets every `XDG_*` variable.
2. It copies `testdata/fixtures/claude-code/session.jsonl` into that home, where `mnemonic local` ingests its default sources.
3. It extracts the archive and runs `mnemonic local`.
4. It sends one MCP `search_sessions` call with the query `inspect sample project`.
5. It passes when the result names the fixture session, `7c666c01-d38e-4658-8650-854ffb5b626e`.

Run it on a snapshot archive from `dist/` that matches your machine.

Never run the smoke with the real `HOME`: `mnemonic local` would ingest the real session corpus. Never run `mnemonic init`, `mnemonic join` or `mnemonic push --watch` as part of a smoke check, in any home. They register the `com.mnemonic.push` launchd label in the current user's real GUI domain.

## Limits

- `mnemonic local` serves only while it runs. Under Homebrew, `brew services start mnemonic` runs it in the background. The archive and installer give no background service.
- The binaries are not code-signed or notarized. A `curl` download leaves no quarantine attribute on the binary, and a browser download of the archive does (see [distribution.md](distribution.md#what-macos-does-to-an-unsigned-binary)). Whether a Homebrew formula install is quarantined is unconfirmed, and step 7 of the runbook checks it.
- Nobody has run the formula or its service on Linux. Step 7 of the runbook is the first run.

## Release day

The runbook for publishing `v0.8.0`. Nothing here has run. Every step belongs to the owner, who holds the repository admin rights, and each step names the result that shows it worked. Do not start a step before the one above it succeeds.

1. **Prune the branches.** Run `scripts/branches.sh`. It prints keep, delete, or ask for every branch on origin. Review the list, then delete the branches marked delete. Then run these commands.

   ```sh
   gh variable set PRUNE_LANE_BRANCHES --body true -R candyflipline/mnemonic
   gh api repos/candyflipline/mnemonic --jq .delete_branch_on_merge
   ```

   Success is a census with no delete or ask row left, and the second command printing `true`. If it prints `false`, run `gh repo edit candyflipline/mnemonic --delete-branch-on-merge`.

2. **Make the repository public.** Run `gh repo edit candyflipline/mnemonic --visibility public --accept-visibility-change-consequences`. Success is `curl -sI https://github.com/candyflipline/mnemonic` answering `200` without a token.

3. **Create the tap.** Run `gh repo create candyflipline/homebrew-tap --public --description "Homebrew tap for mnemonic"`. The repository starts empty, and its default branch must be `main`. Success is `gh repo view candyflipline/homebrew-tap --json visibility --jq .visibility` printing `PUBLIC`. An empty repository has no branch yet. If GoReleaser's first push in step 6 fails for lack of `main`, push an initial commit on `main` to the tap and rerun.

4. **Create the deploy key and store it.** Run these commands in an empty directory outside any repository.

   ```sh
   ssh-keygen -t ed25519 -N "" -C "mnemonic release" -f ./tap_deploy_key
   gh repo deploy-key add ./tap_deploy_key.pub --allow-write --title "mnemonic release" -R candyflipline/homebrew-tap
   gh secret set HOMEBREW_TAP_DEPLOY_KEY < ./tap_deploy_key -R candyflipline/mnemonic
   shred -u ./tap_deploy_key ./tap_deploy_key.pub 2>/dev/null || rm -P ./tap_deploy_key ./tap_deploy_key.pub
   ```

   Success is `gh repo deploy-key list -R candyflipline/homebrew-tap` showing the key with write access, and `gh secret list -R candyflipline/mnemonic` showing `HOMEBREW_TAP_DEPLOY_KEY`. The private half exists only in the secret after the last command.

5. **Push the tag.** From a clean checkout of the commit to release, which is `main` at the head of the CI run you trust, run:

   ```sh
   git tag -a v0.8.0 -m v0.8.0 && git push origin v0.8.0
   ```

   Push this one tag by name. Never run `git push --tags`, because the 32 Rust-seed tags in local clones would publish too. Success is `git ls-remote --tags origin` listing `v0.8.0` and no other tag.

6. **Watch the release.** Find the run with `gh run list --workflow release.yml --limit 1 -R candyflipline/mnemonic`, then run `gh run watch <run id> -R candyflipline/mnemonic`. Success is a green run, the four archives and `checksums.txt` on the release page, a commit on `candyflipline/homebrew-tap` that adds `Formula/mnemonic.rb`, and `gh api repos/candyflipline/mnemonic/releases/latest --jq .tag_name` printing `v0.8.0`. If the gates or the smoke fail, GoReleaser never ran and nothing published. If GoReleaser fails, look at the release page and the tap before you retry. Either way, fix forward on a new commit, delete the failed release and the tag (`git push origin :refs/tags/v0.8.0` and `git tag -d v0.8.0`), and start again at step 5.

7. **Verify on clean machines.** Do this on one Mac and one Linux machine. Use `export HOME=$(mktemp -d)` with every `XDG_*` variable unset, so that no real sessions are indexed. Never run `mnemonic init`, `mnemonic join` or `mnemonic push --watch`. On each machine, check the three install paths.

   ```sh
   brew install candyflipline/tap/mnemonic && mnemonic version && brew test mnemonic
   curl -fsSL https://raw.githubusercontent.com/candyflipline/mnemonic/main/install.sh | sh
   shasum -a 256 --ignore-missing -c checksums.txt
   ```

   Run the `shasum` line in the directory that holds the downloaded archive and `checksums.txt`. Then copy `testdata/fixtures/claude-code/session.jsonl` to `$HOME/.claude/projects/demo/session.jsonl`, run `mnemonic local` in one terminal, and call it from another.

   ```sh
   curl -s http://127.0.0.1:7781/mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H 'Mcp-Protocol-Version: 2025-06-18' -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"inspect sample project"}}}'
   ```

   Success is `mnemonic version` printing `mnemonic 0.8.0 (<short commit>)` on every path, `brew test mnemonic` and `shasum` reporting `OK`, and the `curl` answer listing a hit for session `7c666c01-d38e-4658-8650-854ffb5b626e`. On the Mac, also run `brew services start mnemonic` and confirm `brew services list` shows `mnemonic` as `started`, then stop it.

8. **Land the README commit.** Merge branch `lane/release-day` into `main` and push `main`. That commit adds the install lines to `README.md`, and it must not land before step 7 passes, because the lines fail before the tag exists. Success is `main` carrying the install lines and its `check.yml` and `smoke.yml` runs green.
