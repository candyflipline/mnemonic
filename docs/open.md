# Open items

Production runs the QUIC-only server on one server, deployed with `ops/deploy.sh`. The cutover and operations are in [cutover.md](cutover.md). Every item below is unscheduled until the owner picks it.

## Server and client

- Cleanup after a clean week on the QUIC cutover: the old `MNEMONIC_TOKEN` in the owner's shell profile, `~/.mnemonic/token`, the old token files and the autocert directory on the server. The `arena/` scripts still use tokens.
- OTLP has no forwarder.
- The Mac client dials with one retry (one extra round trip); kept on purpose.
- The old compose directory on the server still names Caddy, and the old ClickHouse image (1.13 GB) is still there. Both are the owner's to delete.
- Five pre-rebase `lane/quic-*` sub-lane branches and `lane/spike-quic` remain on origin as a record.
- A later `[[keys]]` author in another tenant can strand a live member (config collision with a joined member).
- `F4`: the local-mode annotation ask.
- Parser and index: part-format rows (`qlsource.go:287`, `archive.go:707`, `layout.go:28`, `export.go:56`) keep their comments; the `RawEnd==0` shim stays (no server backfill); `section.go:183` corrupt-section refusal was never decided.
- Index follow-ups: append cost on the first `archiveSetFor` (27.9 s), opening a huge session (65 s), nested lists render flat in read links, the markdown work log omits subagent asks, and wide tables wrap narrow columns.
- Arena parity (126 of 126 against 59796bc) cannot be re-run without a parity box, so index refactors since then are test-verified only.

## Paused: shared storage and keeper research

Paused by the owner on 2026-10-23, after the owner decided not to use Tapedrive and to use Hetzner block storage instead. The design is DuckLake-style Parquet on object storage with a per-tenant keeper, in branch `lane/traces` (pushed, last at 4758c19). The plans and research are outside git in `~/.claude/plans/` (`keeper-compute-arch/`, `storage-compute-patterns-2026-10-22/`, `traces-substrate-2026-10-22/`, `tapedrive-deep-dive-2026-10-21/`). Facts worth keeping:

- Of the object stores checked, only Cloudflare R2 documents both conditional writes (`If-Match`, `If-None-Match`) and strong list consistency. Hetzner Object Storage is HDD-backed, tuned for objects of about 1 MB or more, and its conditional PUT is undocumented. Test it with two `PUT If-None-Match:*` calls before relying on it.
- Tapedrive is pre-mainnet and its node and gateway source is unpublished. A tape allows 65,536 lifetime appends, and the gateway cannot take objects over 4 GiB.
- Resume at the cheapest probes: the Hetzner conditional-write test, then DuckLake against the Tapedrive local gateway ETags if Tapedrive returns.
