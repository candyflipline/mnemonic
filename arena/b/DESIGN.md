# Candidate b: immutable Go segments

## Format and publication

Raw remains authoritative. Each source checkpoint records its compressed byte
boundary, a 64 KiB trailing-window SHA-256, parser continuation state, parser
version, manifest identity, and ordered immutable segment names. Appends seek to
that compressed boundary and parse only the appended zstd frames. Source identity
changes, shrinkage, changed checkpoint windows, and parser upgrades cause reparse.

Each newly parsed interval produces an immutable directory:

- `meta.zst`: streaming gob event metadata compressed with zstd level 3. Includes
  previews, text identity, doc length, type flags, tool input and path provenance,
  and heavy-block locators. Metadata-bearing payloads remain here for titles.
- `heavy`: independent zstd frames holding gob arrays of full text, payload JSON,
  and token JSON. Blocks target 256 KiB uncompressed or 128 events; a larger single
  event gets its own block. Each frame has a four-byte compressed-size prefix.
- `terms`: `MNTERM02`, term count, lexically sorted front-coded UTF-8 terms, and
  delta-varint `(local document, term frequency)` postings. Prefix, suffix, and
  posting lengths are uvarints. Each interval has its own dictionary.
- `manifest.json`: the only publication point. A temporary manifest is fsynced,
  renamed, and its parent directory fsynced after all segment files are durable.

The format is private to this disposable prototype. Gob schemas and the format
version are deliberately not a proposed public storage contract.

## Query structures

- `search_sessions`: sorted segment dictionaries plus compact posting bytes,
  local-to-live document maps, global live corpus statistics, and event metadata.
  Terms accumulate in lexical order with the exact current BM25 formula. All
  matching documents are scored; the two existing sort stages, pre-dedupe limit,
  duplicate equivalence, score normalization, and prose are retained.
- `file_attention`: a reversed path/command-token dictionary narrows request
  metadata before the existing exact/suffix/shell predicates run in Go. Raw
  array captures, malformed input, and delimiter-bearing shell tails retain
  conservative candidate fallbacks. Exact matches take precedence
  before deduplication. Roots and project scope retain the original closed scope.
- `open`: UID lookup, ordered session vectors, turn coordinates, and bounded
  heavy-frame reads. The existing navigation, content, cursor, and prose logic
  is retained. A decoded-frame cache retains at most 8 MiB of payload blocks.
- `list_sessions`: ordered session vectors and metadata aggregates with the
  existing filter/cursor/display-label logic.
- `get_ingest_status`: source manifest counters and the existing status derivation.

Metadata strings with repeated identities are interned. Full payloads and full
text are not resident at startup. Terms are not retokenized during batch startup.
The only non-standard runtime module is klauspost/compress; the binary is CGO-free.

## Recovery and freshness

Segments and checkpoints publish together, per source. A killed writer can leave
an unreferenced temporary or completed segment, but cannot publish an incomplete
segment or advance its parser checkpoint independently. A rerun resumes from the
last published checkpoint and reclaims unreachable segment files. Raw is never
modified. Repeated ingest with unchanged input leaves the manifest unchanged.

A store-local advisory lock serializes ingest writers. Queries pin heavy-file
descriptors, so source replacement can reclaim names without breaking readers.
The warm query process checks the atomic manifest before each call and reloads
a changed snapshot. New raw becomes queryable after `ingest` publishes it. There is no raw-tail query merge or
background ingest daemon in this CLI prototype. A production server should wake
ingest on the existing raw PUT signal and apply a short bounded debounce.

Missing record timestamps use one deterministic far-future stamp, as required by
CONTRACT-NOTES. This preserves the oracle's ordering and incremental/crash parity;
only the documented wall-clock fields are masked by the official checker.

## Alternatives and limits

- Kept a first full-event compressed-segment implementation as the baseline; it
  retained large payloads and retokenized text at startup. Lazy heavy blocks and
  persisted postings reduce both startup work and resident memory.
- Rejected SQLite and another embedded SQL engine: unnecessary dependency and
  query translation cost for five fixed tools.
- Rejected an FST: the supplied measurements favor front coding on digit-heavy
  terms. Segment-local dictionaries also keep incremental publication simple.
- Deferred 128-document bitpacking and block-max MAXSCORE: varints are simpler,
  and exact exhaustive postings traversal is already fast at this sample size.
  Top-k pruning must preserve the first-stage tie set and summation order.
- Retained derived heavy payload blocks rather than raw frame locators because
  the provided raw files can be whole-source zstd frames. Raw seekability should
  be introduced upstream before relying on bounded random raw reads.
- The prototype recomputes session ordering and live winner maps at startup.
  It does not compact live append segments or persist analytics rollups.

## At 100x / production scale

Replace resident gob event objects with packed columns and interned integer IDs;
use immutable mmap-backed posting/metadata files and a bounded page budget.
Compact source intervals into tiered segments, with source-generation tombstones
and live df/length accounting. Add block impacts and measured exact MAXSCORE when
long posting lists justify it. Persist session/turn rollups and the reversed path
index (currently built from metadata at startup). Store heavy locators into seekable raw frames to eliminate the duplicated
payload copy. The CLI's per-source publication protocol can remain the recovery
boundary; query generations need pinning while readers finish.
