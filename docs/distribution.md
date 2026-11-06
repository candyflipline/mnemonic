# About distributing mnemonic

This page explains how mnemonic reaches a developer's machine once `candyflipline/mnemonic` is public, and why the channels rank in the order they do. It does not give install steps. The steps belong in the README, and the installer and archive facts are in [docs/release.md](release.md). Making the repository public, pushing a tag, and creating a tap are the owner's calls. This page assumes none of them has a date.

## Recommendation

1. **The `install.sh` installer.** `curl -fsSL https://raw.githubusercontent.com/candyflipline/mnemonic/main/install.sh | sh` needs no extra repository and no token. It works on macOS and Linux the moment a tag has a release. Because `curl` writes the file, macOS does not quarantine the binary [8].
2. **The release archive.** The same URLs the installer reads, downloaded by hand and checked against `checksums.txt`. It serves people who will not pipe a script into `sh`.
3. **`go install -tags server github.com/candyflipline/mnemonic/cmd/mnemonic@<tag>`.** It serves Go users and needs Go 1.26.6 (`go.mod`). Name the tag. `@latest` prefers any release version over a prerelease [7], so it does not reliably reach `v0.8.0-rc.1` (see [release.md](release.md#install-from-a-release)). The `-tags server` flag is required because `mnemonic local` exists only in that build (`cmd/mnemonic/local.go:1`).
4. **A Homebrew formula in `candyflipline/homebrew-tap`, once the owner creates it.** `brew install candyflipline/tap/mnemonic` suits macOS developers who already use Homebrew. It needs a repository and a write token, so it comes last.

Not now: Apple notarization (99 USD per year) and homebrew-core.

While the repository is private, anonymous downloads fail, so the installer cannot run. A collaborator runs `gh release download v0.8.0-rc.1 -R candyflipline/mnemonic -p '*darwin_arm64*'` instead [1][2][3]. That the anonymous failure follows from the two access rules is an inference, not a quoted sentence. Today `https://github.com/candyflipline/mnemonic/releases/download/v0.8.0-rc.1/checksums.txt` and the releases API both answer 404.

## What the release contains

GoReleaser builds one static binary per OS and architecture, `CGO_ENABLED=0` and `-tags server`, about 31 MB, packed as `tar.gz` for darwin and linux on arm64 and amd64, plus a checksums file. The binary is unsigned. The first command a new user runs is `mnemonic local`, which serves MCP at `http://127.0.0.1:7781/mcp` (`cmd/mnemonic/local.go:55`). The users are developers of coding agents, so they have a terminal, `curl`, and `git`.

Two facts decide most of the channel choice. A public repository serves release assets to anyone [2], so the installer needs no credentials. The binary is unsigned, so macOS decides whether to stop it by how the file arrived.

## Why the installer comes first

A public repository is "accessible to everyone on the internet" [2]. Release assets live at `https://github.com/<owner>/<repo>/releases/download/<tag>/<file>`. That shape answers anonymous requests with a redirect to the asset: a request to a public GoReleaser release returned 302, then 200 on 2026-11-06. The GitHub page on release links documents only the `releases/latest/download` form [29].

`releases/latest/download` is the wrong URL for a first release. GitHub defines the latest release as "the most recent non-prerelease, non-draft release" [6], and the first mnemonic release is the prerelease `v0.8.0-rc.1`. So `install.sh` resolves the tag itself. It reads `MNEMONIC_VERSION` if set. Otherwise it asks `https://api.github.com/repos/candyflipline/mnemonic/releases?per_page=1`, which lists prereleases, and takes the first tag. The REST page says published releases "are available to everyone" and does not state the order of the list [6]. Lists from two public repositories were newest first by `created_at` on 2026-11-06, including prereleases, and the installer relies on that observed order.

A `curl | sh` installer asks users to run code they have not read. The archive channel exists for them. The script stays short, takes its checksums from the same release, and stops on a mismatch.

## What macOS does to an unsigned binary

Gatekeeper acts on files that carry the `com.apple.quarantine` attribute. Palo Alto's Unit 42 reports that "Unix-based networking tools, such as SCP and curl, won't mark their downloaded files as quarantined" [8]. A browser does set the attribute. For a quarantined unnotarized app, Apple says that "by default, macOS Catalina and later also requires software to be notarized" and describes the override as **Open Anyway** in **Privacy & Security** [9].

Applied to mnemonic:

- The installer and a `curl` archive download leave no quarantine attribute and raise no prompt. The binary that `install.sh` installed from a local server carried only `com.apple.provenance`, which a local `go build` output carries too, and no `com.apple.quarantine`. That run does not cover a download from GitHub, so check the installer once on a clean Mac after the first release.
- A user who downloads the archive in a browser from the release page gets the quarantine attribute and a block. `Open Anyway` works, but it is a poor first impression for a tool that installs a launchd agent. Prefer the installer or a `curl` download over a browser.

Homebrew changes this by package type.

- **Casks.** A cask download is quarantined. GoReleaser's cask page says that without signing, or without a post-install hook that runs `xattr -dr com.apple.quarantine` on the binary, users may see a "damaged and cannot be opened" alert [5]. The same page warns that this bypasses macOS verification and that Apple may disable the method in a future macOS version [5].
- **Formulae.** One project's pull request states that "formulae do not quarantine" and dropped its workaround for that reason [11]. That is a single project's claim. I found no Homebrew page that says it, so treat it as likely but unconfirmed.

## Why Homebrew means an own tap, and which kind

**homebrew-core is closed to it for now.** The policy asks for at least 30 forks, 30 watchers, or 75 stars, and for a self-submission by the owner at least 90 forks, 90 watchers, or 225 stars. A repository under 30 days old is normally not eligible [12]. Core also wants software that builds from source or installs portable output, and puts "proprietary or platform-specific binary-only software" in a cask [13]. A formula that downloads our prebuilt archive does not fit core. A formula that runs `go build` could, once the stars exist.

**The official cask repository requires a signature.** Homebrew's cask rules say that executable artefacts Gatekeeper can assess "must pass Homebrew's" Gatekeeper checks and "must not require System Integrity Protection or Gatekeeper to be disabled or bypassed" [14]. Homebrew's issue tracker records the plan to end support for casks that fail Gatekeeper checks on 1 September 2026 and to deprecate `--no-quarantine` [15]. A maintainer in a separate thread says Homebrew no longer wants unsigned casks "in our official taps", while anyone may host their own tap [16]. An unsigned binary therefore belongs in our own tap, and the xattr hook is only a stopgap even there.

**An own tap is the cheap path.** Homebrew recommends that a GitHub tap's repository name start with `homebrew-`, so `brew tap candyflipline/tap` works. `brew install user/repository/formula` adds the tap automatically [17]. GoReleaser needs write access to the tap repository. In GitHub Actions the default token cannot push to another repository, so a separate token with content write privileges is required [4][5].

**Formula or cask.** GoReleaser marks `brews` as deprecated "since v2.10 (soft) since v2.16" and says casks "should be used instead" [18]. That argues for `homebrew_casks`. Three things argue against it for mnemonic.

- A cask quarantines the download, so an unsigned binary needs the xattr hook that Apple and Homebrew are moving against [5].
- The cask page's examples are macOS-specific, and I found no source that says casks install on Linux. The two Linux targets may be left without a Homebrew channel (not found, so unconfirmed).
- A formula also lets a later release add `brew services`. The pull request I read says a cask has no `service` stanza [11], which is again one project's account.

I recommend the formula and accepting the deprecation. The deprecations page gives no removal date or version for `brews` [18]. The owner can switch to `homebrew_casks` with `tap_migrations.json` later, and users who tapped the formula upgrade to the cask without action [18]. If the owner decides to sign and notarize, the cask path becomes the better one.

## Notarization, and why it waits

Notarization removes the quarantine problem for everyone, including browser downloads. It costs a yearly fee and CI work.

- **Money.** The Apple Developer Program costs 99 USD per membership year, with fee waivers for eligible nonprofits, accredited educational institutions, and government entities [19].
- **What GoReleaser needs.** Its `notarize.macos` section uses `anchore/quill`, runs on any OS, and works only with binaries and universal binaries. The page lists a Developer ID certificate exported as a `.p12` file with a password, an App Store Connect API key as a `.p8` file with its issuer ID and key ID, and a notarization `timeout` that defaults to 10 minutes and caps at 20. The page names five CI secrets for this, `MACOS_SIGN_P12` among them [20].
- **The native path.** `notarize.macos_native` uses `codesign` and `xcrun`, needs a Mac runner with a keychain, and is marked as GoReleaser Pro [20].
- **What I could not read.** Apple's own notarization page did not load, so I make no claim about `notarytool` details or whether a bare binary can be stapled. Read that page before the owner commits.

The installer and the `curl` archive path avoid the quarantine attribute, so notarization buys nothing for them. It starts to pay when browser downloads and Homebrew casks matter.

## Other channels

The agent-native doors are not install channels, and they stay separate from the binary.

- **Claude Code.** `claude mcp add --transport http <name> <url>` registers a remote HTTP server [21], so `http://127.0.0.1:7781/mcp` connects once `mnemonic local` runs. A plugin can bundle a `.mcp.json` and skills, and a marketplace can be a GitHub repository [22][23]. A plugin does not fetch or run the binary for the user, and its `bin/` directory would need all four platform builds in Git. Build a plugin after the first release, to wire the agent and teach the tools, not to deliver the binary (inference).
- **Codex.** A `[mcp_servers.<name>]` table with a `url` in `~/.codex/config.toml` registers a Streamable HTTP server [24].
- **npm and `npx`.** GoReleaser's `npms` section is Pro-only and the page calls it alpha. Its package runs a `postinstall` that downloads the archive, which does not work with `--ignore-scripts` [25]. It pays only if the audience is mostly JavaScript developers who do not use Homebrew.
- **Nix.** GoReleaser publishes a derivation to a Nix User Repository, with some options Pro-only [26]. It serves a narrow group and adds a repository to maintain.
- **Docker.** The `dockers` section is phased out in GoReleaser v2.12 in favor of `dockers_v2`, whose name the page calls "provisional" and which needs Docker and `buildx` [27][28]. An image suits `mnemonic serve` for a team operator, not the one-command local try-out, and the repository's rules forbid running Docker on the owner's machine.
- **aqua, `mise`, `ubi`.** I did not read the aqua registry's inclusion criteria. `mise` and `ubi` fetch GitHub releases but are tools the user must already have, so they add no reach. Not read, so no claim.

## Tradeoffs

| Rank | Channel | User command | Gatekeeper outcome | Our setup cost | Ongoing cost |
|---|---|---|---|---|---|
| 1 | `install.sh` | `curl -fsSL https://raw.githubusercontent.com/candyflipline/mnemonic/main/install.sh \| sh` | No quarantine [8] | The script in the repository, public repo, a tag | Keep it in step with the archive names |
| 2 | Release archive | `curl` the archive and `checksums.txt`, `shasum -a 256 -c`, `tar -xzf` | No quarantine from `curl` [8]. A browser download is quarantined [9] | None beyond the tag | None |
| 3 | `go install` | `go install -tags server github.com/candyflipline/mnemonic/cmd/mnemonic@<tag>` | None, the user builds it | None | Users need Go 1.26.6 |
| 4 | Own tap, formula (`brews`) | `brew install candyflipline/tap/mnemonic` | Likely not quarantined [11] | One repository, one token, one config section | Track GoReleaser's `brews` deprecation [18] |
| | Own tap, cask (`homebrew_casks`) | `brew install --cask candyflipline/tap/mnemonic` | Quarantined, so needs the xattr hook or a signature [5] | Same, plus the hook | The hook may stop working [5] |
| | homebrew-core | `brew install mnemonic` | Not quarantined if built from source (inference) | Needs 225 stars or 90 forks or 90 watchers [12] | Homebrew maintainers own updates |
| | Claude Code plugin | `/plugin marketplace add candyflipline/<marketplace>` | Not applicable, no binary | A marketplace repository and `.mcp.json` | A second thing to version |
| | npm or `npx` | `npx <package>` | Not read | GoReleaser Pro, alpha [25] | A package to publish |
| | Docker for `serve` | `docker run <image> serve` | Not applicable | `dockers_v2` and a registry [27] | Image per release |

## Decisions for the owner

1. **Visibility and the first tag.** Every channel above waits for the repository to be public and for `v0.8.0-rc.1` to be pushed. Before pushing tags from any clone, list them: a local clone also holds the Rust seed's 32 tags, `v0.1.0` through `v0.7.3`,, and `git push --tags` would publish them (`docs/release.md`).
2. **Whether to pay for notarization.** Default is no for the first release, and yes before a wide launch, because browser downloads and Homebrew's direction both push toward a signature [15][16].
3. **Formula now or cask now.** Default is the formula, for Linux coverage and no xattr hook. Switch if `brews` is removed or if the owner signs.
4. **Tap repository and token.** Naming `candyflipline/homebrew-tap` and issuing a write-scoped token are the owner's. Adding the `brews` section to `.goreleaser.yaml` follows once both exist.
5. **The path `init` records (blocks the tap).** `runningBinary` resolves the running binary with `filepath.EvalSymlinks` (`cmd/mnemonic/init.go:217-226`), and the launchd plist records that resolved path. Under Homebrew it is the versioned Cellar path. `brew upgrade` followed by cleanup deletes that directory, and the push agent then points at a binary that no longer exists. Before the formula channel ships, `init` must record a stable path, such as Homebrew's `opt` prefix. Read from the code, not run, because running `init` is off limits on this machine.

## Sources

Sources 2, 3, 4, 6, 7, 8, 17, 18, and 29, and the token note on source 5, were fetched on 2026-11-06 for this revision. Sources 1, 9, 11 through 16, 19 through 28, and the rest of source 5 were fetched in the earlier session that wrote the first version, and this revision does not change what they support.

1. [About releases](https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases)
2. [About repositories](https://docs.github.com/en/repositories/creating-and-managing-repositories/about-repositories)
3. [gh release download](https://cli.github.com/manual/gh_release_download)
4. [GoReleaser, Homebrew Formulas (deprecated)](https://goreleaser.com/customization/homebrew_formulas/)
5. [GoReleaser, Homebrew Casks](https://goreleaser.com/customization/homebrew_casks/)
6. [GitHub REST API, releases](https://docs.github.com/en/rest/releases/releases)
7. [Go modules reference](https://go.dev/ref/mod)
8. [Unit 42, Gatekeeper bypass on macOS](https://unit42.paloaltonetworks.com/gatekeeper-bypass-macos)
9. [Apple, Safely open apps on your Mac](https://support.apple.com/en-us/102445)
11. [Pull request 36 in blairham/go-pre-commit](https://github.com/blairham/go-pre-commit/pull/36), one project's account
12. [Homebrew, Package Acceptance Policy](https://docs.brew.sh/Package-Acceptance-Policy)
13. [Homebrew, Acceptable Formulae](https://docs.brew.sh/Acceptable-Formulae)
14. [Homebrew, Acceptable Casks](https://docs.brew.sh/Acceptable-Casks)
15. [Homebrew/brew issue 20755](https://github.com/Homebrew/brew/issues/20755)
16. [Homebrew discussion 7050](https://github.com/orgs/Homebrew/discussions/7050)
17. [Homebrew, How to create and maintain a tap](https://docs.brew.sh/How-to-Create-and-Maintain-a-Tap)
18. [GoReleaser, deprecations](https://goreleaser.com/deprecations/)
19. [Apple Developer Program, what's included](https://developer.apple.com/programs/whats-included/)
20. [GoReleaser, notarize](https://goreleaser.com/customization/notarize/)
21. [Claude Code, MCP](https://code.claude.com/docs/en/mcp)
22. [Claude Code, host and maintain a marketplace](https://code.claude.com/docs/en/plugins/host-marketplace.md)
23. [Claude Code, plugin components](https://code.claude.com/docs/en/plugins/components.md)
24. [Codex, MCP](https://learn.chatgpt.com/docs/extend/mcp?surface=cli)
25. [GoReleaser, npm](https://goreleaser.com/customization/npm/)
26. [GoReleaser, Nix](https://goreleaser.com/customization/nix/)
27. [GoReleaser, dockers_v2](https://goreleaser.com/customization/dockers_v2/)
28. [GoReleaser, Docker](https://goreleaser.com/customization/docker/)
29. [GitHub, Linking to releases](https://docs.github.com/en/repositories/releasing-projects-on-github/linking-to-releases)

Not read, so not cited for any claim. Apple's notarization page (`developer.apple.com/documentation/security/notarizing-macos-software-before-distribution`, returned only a title), the aqua registry inclusion policy, the `mise` and `ubi` pages, and any Homebrew page on casks under Linux.
