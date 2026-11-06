# About distributing mnemonic

This page explains how mnemonic reaches a developer's machine, and why the first test release and the public release use different channels. It does not give install steps. The steps belong in the README and in `docs/release.md`.

## Recommendation

- **Private test.** Invited testers get read access to the repository and run `gh release download` on the GoReleaser archive. No tap, no new repository, no signing.
- **After the repository is public.** Publish a Homebrew formula in an own tap, `candyflipline/homebrew-tap`, from GoReleaser's `brews` section. Keep `gh release download` and a plain archive link as the fallback for Linux and for people without Homebrew.
- **Not now.** Apple notarization ($99 per year) and homebrew-core.
- Going public is the owner's call. This page assumes nothing about its date.

## What the release contains

GoReleaser builds one static binary per OS and architecture, `CGO_ENABLED=0` and `-tags server`, about 31 MB, packed as `tar.gz` for darwin and linux on arm64 and amd64, plus a checksums file. The binary is unsigned. The first command a tester runs is `mnemonic local`, which serves MCP at `http://127.0.0.1:7781/mcp` (`cmd/mnemonic/local.go:55`). The users are developers of coding agents, so they have a terminal, `git`, and usually `gh` already.

Two facts decide most of the channel choice. The repository is private, so nobody outside it can download its assets. The binary is unsigned, so macOS decides whether to stop it by how the file arrived.

## Why the private window needs no tap

GitHub's rule for releases is that "anyone with read access to a repository can view and compare releases" [1]. A private repository is "only accessible to you, people you explicitly share access with" [2]. Release assets carry no separate rule on the pages read, so anonymous downloads of a private repository's assets fail. That conclusion is an inference from those two statements, not a quoted sentence.

A tester added as a collaborator therefore has everything needed, and `gh release download` takes a repository, a tag, and a glob pattern for the asset [3]. This costs the owner nothing beyond the invitation. It also keeps the first test at a size where the owner can talk to each tester.

Each alternative while private adds a repository or a token, and the owner has not approved either.

- **A public binaries-only repository or a public tap that holds the archives.** Anonymous downloads work, but it publishes the binary, which is most of what going public means. It also creates a repository, which this release does not do.
- **A private tap with a custom download strategy.** GoReleaser's documented route for a private source repository is a `CurlDownloadStrategy` subclass in `custom_block`. Each user must export `HOMEBREW_GITHUB_API_TOKEN`, and the example uses a Homebrew internal API that the page says you must replace [4]. Homebrew's own manual describes that variable as a personal access token for the GitHub API [5]. A tester who can already run `gh` has no reason to take on this setup.
- **`mise`, `ubi`.** Both can fetch from private releases with a token. mise's `github:owner/repo` backend says public releases need no token and points to a token page for private ones [6]. ubi states that a `GITHUB_TOKEN` "is required to download releases for a private project" [7]. They are tools the tester must already have, so they add no reach.
- **`go install`.** It needs a module the Go toolchain can fetch. For a private module the toolchain skips the proxy and the checksum database when `GOPRIVATE` matches, which makes each tester configure Git credentials and set an environment variable (`go help private`, Go 1.26.6, run locally). Users also build from source, which takes a Go toolchain and loses the `mnemonic version` stamp (inference, since the stamp comes from GoReleaser's ldflags).

## What macOS does to an unsigned binary

Gatekeeper acts on files that carry the `com.apple.quarantine` attribute. Palo Alto's Unit 42 reports that Unix networking tools such as `curl` and `scp` "won't mark their downloaded files as quarantined", and that a file without the attribute skips the check [8]. A browser does set it. For a quarantined unnotarized app, Apple says that "by default, macOS Catalina and later also requires software to be notarized" and describes the override as **Open Anyway** in **Privacy & Security** [9].

Applied to mnemonic, which is inference built on those two sources:

- A tester who runs `curl`, then `tar`, gets no quarantine attribute and no prompt. For `gh release download` and for `tar` extraction I found no source, which is the one gap. Check the tester path once on a clean Mac before the release is announced.
- A tester who downloads the archive in a browser from the release page gets the quarantine attribute and a block. The `Open Anyway` override works, but it is the wrong first impression for a tool that installs a launchd agent.

Homebrew changes this by package type, and the difference matters more than it first looks.

- **Casks.** A cask download is quarantined. GoReleaser's cask page says that without signing, or without a post-install hook that runs `xattr -dr com.apple.quarantine` on the binary, users may see a "damaged and cannot be opened" alert [10]. The same page warns that this bypasses macOS verification and that Apple may disable the method in a future macOS version, and offers `caveats` as a way to tell users to run the command themselves [10].
- **Formulae.** One project's pull request states that "formulae do not quarantine" and dropped its workaround for that reason [11]. That is a single project's claim. I found no Homebrew page that says it, so treat it as likely but unconfirmed. It matches the curl behaviour above, since Homebrew formulae are downloaded by command-line tooling (inference).

## Why Homebrew means an own tap, and which kind

Homebrew has two homes for a package, and mnemonic fits neither the official one nor the cask path cleanly.

**homebrew-core is closed to it for now.** The policy asks for at least 30 forks, 30 watchers, or 75 stars, and for a self-submission by the owner at least 90 forks, 90 watchers, or 225 stars. A repository under 30 days old is normally not eligible [12]. Core also wants software that builds from source or installs portable output, and puts "proprietary or platform-specific binary-only software" in a cask [13]. A formula that downloads our prebuilt archive does not fit core. A formula that runs `go build` could, once the stars exist.

**The official cask repository requires a signature.** Homebrew's cask rules say that executable artefacts Gatekeeper can assess "must pass Homebrew's" Gatekeeper checks and "must not require System Integrity Protection or Gatekeeper to be disabled or bypassed" [14]. Homebrew's issue tracker records the plan to end support for casks that fail Gatekeeper checks on 1 September 2026 and to deprecate `--no-quarantine` [15]. A maintainer in a separate thread says Homebrew no longer wants unsigned casks "in our official taps", while anyone may host their own tap [16]. So an unsigned binary belongs in our own tap, and the xattr hook is only a stopgap even there.

**An own tap is the cheap path.** Homebrew recommends that a GitHub tap's repository name start with `homebrew-`, so `brew tap candyflipline/tap` works, and `brew install user/repository/formula` adds the tap automatically [17]. GoReleaser needs write access to that repository, and in GitHub Actions the default token does not work, so a separate token with content write access is required [10].

**Formula or cask.** GoReleaser marks `brews` as deprecated "since v2.10 (soft) since v2.16" and says casks "should be used instead" [18]. That argues for `homebrew_casks`. Three things argue against it for mnemonic.

- A cask quarantines the download, so an unsigned binary needs the xattr hook that Apple and Homebrew are moving against [10].
- The cask page's examples are macOS-specific, and I found no source that says casks install on Linux. Half of the matrix, linux arm64 and amd64, may be left without a Homebrew channel (not found, so unconfirmed).
- A formula also lets a later release add `brew services`. The pull request I read says a cask has no `service` stanza [11], which is again one project's account.

I recommend the formula and accepting the deprecation. GoReleaser has not set a date for removing `brews` (the deprecations page gives only "soft" and "since" versions) [18]. The owner can switch to `homebrew_casks` with `tap_migrations.json` later, as that page describes, without breaking users who tapped it [18]. If the owner decides to sign and notarize, the cask path becomes the better one.

## Notarization, and why it waits

Notarization removes the quarantine problem for everyone, including browser downloads. It costs a yearly fee and CI work.

- **Money.** The Apple Developer Program costs 99 USD per membership year, with fee waivers for eligible nonprofits, accredited educational institutions, and government entities [19].
- **What GoReleaser needs.** Its `notarize.macos` section uses `anchore/quill`, runs on any OS, and works only with binaries and universal binaries. The page lists a Developer ID certificate exported as a `.p12` file with a password, an App Store Connect API key as a `.p8` file with its issuer ID and key ID, and a notarization `timeout` that defaults to 10 minutes and caps at 20. The page names five CI secrets for this, `MACOS_SIGN_P12` among them [20].
- **The native path.** `notarize.macos_native` uses `codesign` and `xcrun`, needs a Mac runner with a keychain, and is marked as GoReleaser Pro [20].
- **What I could not read.** Apple's own notarization page did not load, so I make no claim about `notarytool` details or whether a bare binary can be stapled. Read that page before the owner commits.

For a private test with a handful of invited developers who download through `gh`, this buys nothing. The money starts to pay when browser downloads and Homebrew casks matter, which is the public release.

## Other channels

The agent-native doors are not install channels, and they stay separate from the binary.

- **Claude Code.** `claude mcp add --transport http <name> <url>` registers a remote HTTP server [21], so `http://127.0.0.1:7781/mcp` connects once `mnemonic local` runs. A plugin can bundle a `.mcp.json` and skills, and a marketplace can be a private GitHub repository that testers reach through their own Git credentials [22][23]. A plugin does not fetch or run the binary for the user, and its `bin/` directory would need all four platform builds in Git. Build a plugin after the first release, to wire the agent and teach the tools, not to deliver the binary (inference).
- **Codex.** A `[mcp_servers.<name>]` table with a `url` in `~/.codex/config.toml` registers a Streamable HTTP server [24].
- **npm and `npx`.** GoReleaser's `npms` section is Pro-only and the page calls it alpha. Its package runs a `postinstall` that downloads the archive, which does not work with `--ignore-scripts` [25]. It would also need public assets (inference). It pays only if the audience is mostly JavaScript developers who do not use Homebrew.
- **Nix.** GoReleaser publishes a derivation to a Nix User Repository, with some options Pro-only [26]. It serves a narrow group and adds a repository to maintain.
- **Docker.** The `dockers` section is phased out in GoReleaser v2.12 in favor of `dockers_v2`, whose name the page calls "provisional" and which needs Docker and `buildx` [27][28]. An image suits `mnemonic serve` for a team operator, not the one-command local try-out, and the repository's rules forbid running Docker on the owner's machine.
- **aqua.** I did not find the registry's inclusion criteria. The pages I read link to a support policy without quoting one. Not found, so no claim.
- **A `curl | sh` installer.** It is the standard answer for Linux, and it avoids the quarantine attribute for the reason in the Gatekeeper section. It needs a script that picks the archive, verifies the checksum, and puts the binary on `PATH`. That script lives in the repository and reads release assets, so it works only after the repository is public (inference). It also asks users to run code they have not read.

## Tradeoffs

| Channel | Works while private? | User command | Gatekeeper outcome | Our setup cost | Ongoing cost |
|---|---|---|---|---|---|
| `gh release download` | Yes, collaborator with `gh` | `gh release download -R candyflipline/mnemonic -p '*darwin_arm64*'`, then `tar -xzf` | No quarantine from a command-line download (inference, check once) | None beyond the invitation | None |
| Own tap, formula (`brews`) | Needs a private-repo download strategy and a token | `brew install candyflipline/tap/mnemonic` | Likely not quarantined [11] | One repository, one token, one config section | Track GoReleaser's `brews` deprecation |
| Own tap, cask (`homebrew_casks`) | Same as the formula | `brew install --cask candyflipline/tap/mnemonic` | Quarantined, so needs the xattr hook or a signature [10] | Same, plus the hook | The hook may stop working [10] |
| homebrew-core | No | `brew install mnemonic` | Not quarantined if built from source (inference) | Needs 225 stars or 90 forks or 90 watchers [12] | Homebrew maintainers own updates |
| `curl \| sh` | No, until public | `curl -fsSL <url> \| sh` | No quarantine [8] | An install script with checksum check | Keep it in step with archive names |
| `go install` | Only with `GOPRIVATE` and Git credentials | `go install -tags server .../cmd/mnemonic@latest` | None, the user builds it | None | Users need Go 1.26.6 and lose the version stamp (inference) |
| `mise` or `ubi` | Yes, with a token [6][7] | `mise use github:candyflipline/mnemonic` | Not read | None | Users must already have the tool |
| Claude Code plugin | Yes, through Git credentials [22] | `/plugin marketplace add candyflipline/<marketplace>` | Not applicable, no binary | A marketplace repository and `.mcp.json` | A second thing to version |
| npm or `npx` | Postinstall needs public assets (inference) | `npx <package>` | Not read | GoReleaser Pro, alpha [25] | A package to publish |
| Docker for `serve` | Needs a registry login | `docker run <image> serve` | Not applicable | `dockers_v2` and a registry [27] | Image per release |

## Decisions for the owner

1. **When the repository goes public.** This is owner call 2 in `docs/design-open-source-at-scale.md`. Default is to stay private for the first test and revisit after testers report. The tap and the `curl | sh` script wait for it.
2. **Whether to pay for notarization.** Default is no for the private test, and yes before a wide public launch, because browser downloads and Homebrew's direction both push toward a signature [15][16].
3. **Formula now or cask now.** Default is the formula, for Linux coverage and no xattr hook. Switch if `brews` is removed or if the owner signs.
4. **Tap repository and token.** Naming `candyflipline/homebrew-tap` and issuing a write-scoped token are the owner's.
5. **The path `init` records.** `README.md` says the scheduler runs the binary at the path `init` runs from. A Homebrew install puts the binary under a versioned directory behind a symlink, so the owner should confirm which path `init` writes into the launchd plist before the tap ships (not verified, I did not run `init`).

## Sources

Every URL was fetched in the session that wrote this page, except where a row says not read.

1. [About releases](https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases)
2. [About repositories](https://docs.github.com/en/repositories/creating-and-managing-repositories/about-repositories)
3. [gh release download](https://cli.github.com/manual/gh_release_download)
4. [GoReleaser, Homebrew](https://goreleaser.com/customization/homebrew/)
5. [Homebrew manual](https://docs.brew.sh/Manpage)
6. [mise, github backend](https://mise.jdx.dev/dev-tools/backends/github.html)
7. [ubi](https://github.com/houseabsolute/ubi)
8. [Unit 42, Gatekeeper bypass on macOS](https://unit42.paloaltonetworks.com/gatekeeper-bypass-macos)
9. [Apple, Safely open apps on your Mac](https://support.apple.com/en-us/102445)
10. [GoReleaser, Homebrew Casks](https://goreleaser.com/customization/homebrew_casks/)
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

Not read, so not cited for any claim. Apple's notarization page (`developer.apple.com/documentation/security/notarizing-macos-software-before-distribution`, returned only a title), the Go modules reference section on private modules (cut off), the aqua registry inclusion policy, and any Homebrew page on casks under Linux.
