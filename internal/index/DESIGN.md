# internal/index: mnemonic's own index, no DuckDB

The engine from lane/traces `arena/trace` (4758c19), moved without the keeper,
router, object store, restore, router phases or D2 roots. It is pure Go
(CGO_ENABLED=0); its only non-std dependency is klauspost/compress for zstd.
The parsers are the repo's own (`internal/normalize`). The MCP tools live in
this package, one file per tool (`q_*.go`), and read the parts directly.

## Lies table

| Lie underneath | The one fix | Deleted |
|---|---|---|
| Tools need a SQL handle | `Tool.Call(ctx, *index.DB, args)`; `mcp.Handler` takes `*index.Index` and pins one snapshot per call | `*sql.DB` in mcp and server, `BusyError` and its 429 |
| Raw becomes parquet before it is indexed | `Index.Ingest` parses raw straight into frame logs and parts | lake and converge in `internal/server` |
| An input is always a store raw | a source `Kind` (store zst, plain file in place); only open, the resume byte test and the reader differ by kind | nothing yet (U10 fills the plain kind) |
| Scope is SQL predicates | `SessionFilter` over session ids, applied where each tool already filters rows | `Listed/Opened` SQL strings in the engine path |

## On-disk formats

```
S/src/<author__machine__harness__srcid>.ckpt    JSON, tmp+fsync+rename: the source's commit point
S/src/<key>.<gen>.light | .heavy                append-only zstd frame logs
S/parts/<fingerprint>/                          one immutable index part (below)
S/parts/<id>/deltas/...                         rows appended to a hot part's base (delta.go)
S/CURRENT                                       JSON {parts, hot, sources}, tmp+fsync+rename: the index commit point
```

S is `layout.IndexDir(dataDir)` (`<data>/index`) under `mnemonic serve`.

- **Frame log.** u32 length, u32 count, then zstd-3 records (128 KB light,
  256 KB heavy frames). Light (`row.go`) holds every column but payload_json
  and token_usage_json, which are heavy: read only by `open` and session_meta.
- **Checkpoint** (`ingest.go`): identity, source `Kind`, the harness parser's
  `Version()`, bytes consumed in the kind's own bytes (zst for KindStore,
  plain for KindPlain) and the SHA-256 of the 64 KB before them, parser
  `State`, generation, valid lengths of both logs, and the source's sorted
  session ids.
- **Part** (`build.go`). Fixed-width little-endian arrays, all mmapped:
  `docs` (40 B per event, in session window order), `sessions`, `turns`,
  `uids`, `terms` + `terms.idx` + `terms.filter` + `postings` (front-coded
  blocks of 16, 2 KiB bloom), `hot` (one uncompressed hit record per doc),
  `fa.exact`, `fa.tio`, `fa.blob`, `fa.tri`, `ls.json` rollups and `meta.json`.

## Which structure serves each tool

- **search_sessions.** df per term summed over every part's dictionary, N and
  total_len from the part metas (the cache's exact idf over unfiltered df).
  Each part scores postings through `docs` flags and scope; the global top
  n+1 cut keeps ties; survivors read `hot`, `turns` and `sessions`.
- **file_attention.** Exact pass over `fa.exact`; suffix pass through the
  trigrams of the path's longest literal run in `fa.tri`, then main's
  `bytes.Contains` and regex logic. Tail resolution and worktree roots are
  main's (`q_attention_tail.go`, `q_list_helpers.go`: the local filesystem's
  `.git`), not D2's pushed roots.
- **open.** The session → part map or a `uids` probe picks the part; the scan
  decodes that session's light frames in order.
- **list_sessions / get_ingest_status.** Parts' `sessions` + `ls.json`;
  `CURRENT` source rows (control harness rows skipped, as main does).

## Ingest, crash and recovery

1. **Parse.** Sources parse on GOMAXPROCS workers, one source per worker,
   each with only its own two logs and checkpoint. The harnesses parsed are
   exactly those `normalize.For` knows (transcripts, otlp, annotation), as the
   lake did; `control` has no parser and stays raw only.
   - Resume requires the same kind, identity and parser `Version()`, a State
     that is not `Provisional()`, bytes consumed within the input, and the
     kind's byte test: for KindStore an unchanged window hash and zstd magic
     next; for KindPlain an unchanged window hash of the plain file.
   - Otherwise the generation is bumped and the source re-parses from byte 0.
   - Logs are truncated to the checkpoint's lengths, appended, fsynced, then
     the checkpoint is renamed into place.
2. **Plan.** Union-find joins sources that share a session id. A component
   with a hot source is a hot part of its own; settled components go to the
   cold part of shard `fnv32(least session id) % 256`. A part's name is the
   fingerprint of what its sources parsed.
3. **Build only what is missing**, in `<id>.tmp`, fsynced, renamed. Winners:
   the highest version per uid within a source, then the last source in lake
   path order across sources; then session order, turn_seq, ordinals, rollups.
4. **Commit.** `CURRENT` is rewritten only if its bytes changed, so ingest is
   idempotent; GC then removes parts and log generations it does not name.

Rows with no record_ts get one fixed stamp (2100-01-01), never the wall clock.
The plan rests on one assumption: a uid belongs to one session.

## Hot and cold

A source is hot while `zst_size != cold_zst` in its checkpoint. It settles
at the first ingest where its manifest `updated_ms` is at least the quiet
window (serve: 15 min) behind the newest `updated_ms`, or with `SettleAll`.
An append rebuilds only its component's hot part (`delta.go` appends rows past
the base and refuses past foldRatio 1); a cold part is rewritten only when a
component settles into it or leaves it. Not bounded: a long active session
rebuilds its whole hot part; a component joining a hot source to a large
settled one is hot as a whole.

## Serving

`Index` holds the DB of the CURRENT it last opened. `Snapshot` pins it for
one call; `Refresh` (after every `Index.Ingest`, and before every harness
batch line) opens a newer CURRENT and closes the old DB once its last pin is
released. A failed open keeps the old DB serving.

## Local mode (redact at ingest)

`mnemonic local` hands its ingest a redactor (`IngestOptions.Red`, built as
push builds it: `redact.New(nil)` — local mode has no config file, so no
extra patterns). In `ingestSource` the parser's reader is wrapped
(`redactReader`, kind.go): every line the parse reads goes through
`redact.Line`, the exact bytes push's `redactLines` uploads for the same
lines. The redaction sits on the reader, not the kind: the write tools'
records (KindStore under the data dir) need it as much as the harness files
do, because the tools' store path does not redact — `q_annotate.go:158`
appends the annotation value verbatim (found on this lane; the packet
assumed it was redacted). Server mode hands no redactor: its raw was
redacted by push before it arrived, so one behaviour in both modes — redact
at ingest — and the parser never differs.

**Offsets and uids equal the server path's by construction, no second
offset kept.** `EventUID` hashes the parser's running byte offset
(`shared/hash.go:15`, via `SourceOffset`, shared/jsonl.go), which counts the
bytes the parser has actually read. Local's parser reads the redacted
stream, byte-identical to what the server path's raw holds for the same
file (push redacted each whole line with the same redactor before upload;
`redact.Line` never introduces or removes a newline, so line n maps to line
n). Line numbers match, fingerprints of the line match, so uids match. The
checkpoint's `ZstSize` stays in the plain file's bytes and `WindowSHA` the
plain file's window (kind.go `plainExtends`), so resume works on the
unredacted file; the ckpt's `State.Offset` is parser-internal and counts
redacted bytes, which stays consistent across resumes because both offsets
advance one whole line at a time — `ingestSource` only ever resumes at a
newline (`openPlain`'s end) or a frame boundary (store raw). A redactor
change across runs would shift resumed state; local mode pins one build
(`redact.New(nil)`), so the input cannot happen.

**One key, one source.** The data dir's raw/ can hold copies of harness files:
old `mnemonic local` pushed every file it indexed into raw/ under the same key
(author, machine, src id of the file path) the plain source carries. A list
holding one key twice would run two `ingestSource` workers over one checkpoint
and one pair of logs — each truncating what the other appends, and the kind
mismatch re-parsing every lap — so `IngestPlain` drops the KindStore source
behind a live plain file: the file on disk owns the key, one checkpoint, and
the raw copy stays on disk untouched. A copy whose plain file no longer exists
(retention deletes transcripts) keeps the key to itself and is ingested from
raw: it is the only copy left. `ingest` refuses any list still holding a
duplicate key, with the key in the error, so the race cannot return by another
path.

## Decisions (U0)

1. **Tools in this package.** The tools read part internals (docs, hot
   records, frame cache). An exported read API for `internal/query` would be
   a layer that exists only for the other package. One file per tool keeps
   lanes disjoint; each tool registers itself in `init`.
2. **Handle.** Tools take `*index.DB` (one pinned snapshot); `internal/mcp`
   and `internal/server` take `*index.Index`. ctx carries the identity
   (`auth.From`), the store (`store.WithStore`) and the session controls
   (`index.WithControls`), as main did.
3. **Derived Oct 22 relations are part sections built at part build time**
   (spans, annotations, session authors): a part holds every row of its
   sessions, winners and losers, so each section is a function of the part's
   members and is rebuilt exactly when the part is. Query time only reads.
4. **Harness.** A hidden subcommand, `mnemonic index ingest|batch -root R
   -store S` (`cmd/mnemonic/index.go`), CGO off, arena/a's line protocol.
   arena/check needs a binary it can call as `BIN ingest|batch`, so a one-line
   wrapper script `exec mnemonic index "$@"` is passed as `-bin`.
5. **query tool.** Stubbed on raw args (`q_query.go`); U6 parses them into
   `ql.Query` at the boundary, since `internal/ql` is not on this branch.
6. **Controls.** `controls.go` is a copy of `internal/query/controls.go`;
   the query copy stays only for `cmd/mnemonic/export.go` until U5-export,
   and goes with `internal/query` in U8.

## Seeded contracts

Every stub panics with `lane <unit>`. Oracle is main 59796bc unless named.

| Unit | File | Signature | Must produce | Oracle |
|---|---|---|---|---|
| U3 | `cycle.go` | `func (ix *Index) LastCycle() CycleStats` | log bytes, part bytes, parts built, hot/cold counts written by the last `Ingest` | a 10-row append writes O(component), not O(corpus): `delta.go` path taken |
| U3 | `internal/server/server.go` | `cycle(reason)` | parser-version sweep bounded per cycle (the lake's `Sweep` budget), cycle log of `LastCycle` | 59796bc server cycle timings |
| U5-authors | `authors.go` | `buildSessionAuthors(members []ckpt, rows []built) map[string][]string`; `(db *DB) sessionAuthors(id) []string` | per session, sorted distinct authors of every source emitting a row of it, winner or not; stored as a part section | `session_authors` from `internal/cache/index_session_authors.go` |
| U5-controls | `q_set_visibility.go`, `q_delete_session.go` | `Tool.Call` | main's `internal/query/controls_tools.go` answers, records appended via `Controls.record`; `actor` reads `sessionAuthors` | 59796bc e2e |
| U5-spans | `spans.go` | `buildSpans(harness string, session []*Row) []schema.Span`; `(db *DB) sessionSpans(id) []schema.Span` | spans per harness as `internal/cache/index_spans_*.go`, part section | 59796bc `spans` rows |
| U5-spans | `q_get_trace.go` | `Tool.Call` | main's `internal/query/get_trace.go` over `sessionSpans` + `sessionAnnotations`, scoped by `Opened` | 59796bc get_trace |
| U5-annotations | `annotations.go` | `buildAnnotations(rows []*Row) []schema.Annotation`; `(db *DB) sessionAnnotations(id) []schema.Annotation` | one per annotation_id, as `internal/cache/index_annotations.go`, part section | 59796bc `annotations` rows |
| U5-annotations | `q_annotate.go` | `Tool.Call` | main's `internal/query/annotate.go`; `requireTarget` over the engine | 59796bc annotate |
| U5-export-{atif,otlp,dataset} | `export.go` | `(db *DB) EachSession(hidden map[string]bool, fn func(ExportSession) error) error` | sessions in id order, winner rows in event order with heavy columns, for `internal/export` | 59796bc exports byte-identical |
| U6-query | `q_query.go` | `Tool.Call` on raw args | parse once into `ql.Query`, run over engine rows of events/sessions/spans | `internal/ql` contract; tools.json `query` entry |
| U10 | `kind.go` | `openPlain(s source) (*os.File, int64, error)`; `plainExtends(f *os.File, end int64, prev ckpt) bool` | open the harness file in place, end = last newline; resume iff the 64 KB plain window before `prev.ZstSize` hashes to `prev.WindowSHA`, else parse from 0 into gen+1 | a plain ingest answers like the store ingest of the same file |
| U10 | `kind.go` | `listPlainSources(globs []string, author, machine string) ([]source, error)` | KindPlain sources with push's src ids and manifest fields | push's manifest for the same file |
| U10 | `internal/mcp/mcp.go` | — (deleted) | the output-boundary redaction (redactResult, redactEnvelope, redactValue, redactToolError) is gone; `Handler` takes no redactor — both modes redact at ingest | the local-mode redaction test in `ingest_local_test.go` |
| U10 | `cmd/mnemonic/local.go` | `runLocal(ctx, opts, out, logOut) error` | in-place index redacting at ingest, push.Watch events call ingest, MCP with `Handler(ix, index.Lookup)` | standing predicate 7 |

The five old tools are ported with main's Oct 22 scope (`scope.go`); there
are no U4 stubs. U4 lanes own their tool's parity diffs (see the U0 report).

## Archive kind (U11)

`mnemonic import-ch` wrote the retired ClickHouse events into lake-shaped
parquet under `events/author=<a>/machine=archive-ch/harness=<h>/ch-<export>.parquet`
(59796bc `internal/lake/lake.go:217,240-246`); on prod that is 16 files,
14.3 GB, 4,495 sessions / 6,759,894 rows with no raw copy. The engine reads
those files in place as source kind `KindArchive`, with `github.com/parquet-go/parquet-go`
as its only new direct dependency (pure Go; chosen over the arrow-go that
duckdb already drags in transitively because the engine needs a row reader,
not a columnar analytics runtime). The kind and its functions live in
`archive.go`; `kind.go` (U10's file) is not modified — nothing above ingest
looks at the kind (kind.go:10-12).

**Member granularity is (file, session_id) (foreman amendment, Oct 24).**
One ch-* file holds up to ~2,800 sessions and ~6.2M rows; as one member it
would union all of them, plus every raw source sharing any of them, into one
component, so the lies-table bound — an append rebuilds only the hot part of
the touched component, bounded by the component, not the corpus — would be
broken: a 10-row append to such a raw source would rebuild a hot part that
carries millions of archive rows, and the 256-way cold sharding would put one
giant component into one shard. So one pass over a changed file streams its
rows into per-session members (`sourceKey(file) + "~" + session_id`; uuids
never carry `~`), each member's ckpt carrying the parent file's identity
(size and window): an unchanged file is skipped after reading only its size
and last window, and a changed file re-parses all of its members in one pass.
Components stay session-sized exactly as for raw.

### (a) Discovery

`listArchiveSources(root)` globs `<root>/events/author=*/machine=archive-ch/harness=*/ch-*.parquet`
— the machine segment pinned to `layout.ArchiveMachine` because import-ch
writes nowhere else (lake.go:243) — and appends one source per file to
`listSources`' result (ingest.go:479), key `{path author, archive-ch, path
harness, ch-<base>}` so `sourceKey` (ingest.go:92-94) names the file, and
`wantCkpt`'s `Path` (ingest.go:219, layout.go:32-34) is the file's exact lake
path. Discovery is per file; the members are not discovered from the tree but
from the file's checkpoint, which names the member sessions and expands into
them in `publish` (d below). This is the cache's own discovery: `EventsGlob`
matches ch-* names (layout.go:20) and `diffLake` notices a file by size+mtime
(cache index.go:349-352,367-371).

The engine's identity is size + bytes, not mtime, because import-ch rewrites
whole files on rerun (lake.go:216-218): a ckpt's `ZstSize` is the file size
and `WindowSHA` the SHA-256 of its last 64 KB (ingest.go:190-197). An
unchanged file on the next ingest is `Unchanged` (archive.go, the kind's byte
test): no log append, same member bytes → same part fingerprint
(build.go:32-38) → the part exists and is not rebuilt (ingest.go:717-723),
and CURRENT's bytes are equal so it is not rewritten (ingest.go:645-647). A
changed file (any size change or window change) re-parses in one pass into
the members' next generation (archive.go `ingestArchive`); KindArchive's byte
test is "same size AND same window" — no zstd-magic check (storeExtends's,
ingest.go:235-246), a parquet file is never appended.

### (b) Row mapping

Rows stream one row group at a time (`NewGenericRowGroupReader[chRow]` over
each `RowGroup`, parquet-go; groups are 64 MB, lake.go:325,608-610) into the
typed `chRow` whose parquet tags the probe pinned, then go
through `rowFrom` (row.go:109-160) exactly as raw rows do: the two MAP columns
marshal into `Buckets`/`Native` JSON (row.go:129-136), emission_index/tool_io/
meta bits derive from payload_json (row.go:144-158), `TimeUs` from the
record_ts VARCHAR and `TsMs` from event_ts (row.go:137-143). The probe fixed
the decode shapes: LIST needs parquet-go's `,list` struct tag (DuckDB writes
the 3-level `content_types list element`, standard MAPs decode into Go maps
untagged), DATE is int32 days → "2006-01-02" (events.go:11-13), event_ts and
ingested_at are int64 micros with `isAdjustedToUTC=false` → read as UTC, which
is what the cache's session `TimeZone='UTC'` pins (sql.go:54-59, index.go:229).

Dropped, the same columns `rowFrom` never copies because `Row` has no fields
for them (row.go:14-40) — the raw path drops them identically: session_date,
source_inode, origin_tool_call_id, coord_group_id, coord_group_label,
retry_count, service_tier, content_types, has_reasoning. Archive-only
columns: `author` (stamped = path author, lake.go:633-644) is taken from the
ckpt instead (b below); `machine_id` is the literal "archive-ch" (lake.go:639)
and `ingested_at` the epoch literal (lake.go:31,641) — the engine's rows carry
neither field and no tool reads them from events (the cache tools read
machine_id/updated_ms from `sources` only, get_ingest_status.go:58-60); the
`machine` column is the hive partition, not in the files (the cache reads
`hive_partitioning=false`, sql.go:66-73). `payload_json` and `token_usage_json`
go to the heavy log like every source (ingest.go:305-308).

### (c) uid and dedup with raw-derived rows

The cache picks one winner per event_uid across all lake files: `row_number()
OVER (PARTITION BY event_uid ORDER BY event_version DESC, <non-archive first>,
ingested_at DESC, lake_file, lake_row) = 1` (index.go:231,233,572);
event_version is the parse wall clock (normalize/shared/pipeline.go:306), so
the rule is "the most recently parsed copy wins; ties → non-archive; ties →
later ingested_at; ties → path". import-ch also never imported rows whose
source_file already lived in a non-archive parquet of that author
(lake.go:532-556,587-588), so overlap copies exist only where raw landed after
the import ran (2,358 sessions on the box).

The engine compares per source, not per row: highest `Version` within a source
(build.go:116-122), greatest `parsed.Path` across sources (build.go:123-129).
`Version` on raw rows is log place (ingest.go:303) — incomparable with an
event_version — so the cross-source compare becomes kind-aware: **a raw member
beats an archive member of the same uid unconditionally; archive vs archive
falls back to greater Path.** That matches the cache wherever the answers can
differ: the copies' content differs observably only when raw re-parsed at a
newer parser version (higher event_version, raw wins in the cache too), and at
equal versions the cache hands the tie to the non-archive copy
(index.go:231,233) whose content equals the archive copy's (same parser era)
— with ingested_at DESC agreeing (parse wall clock vs the 1970 literal,
lake.go:31; the open path's own tie-break, lake_heavy.go:91, agrees the same
way). Archive rows keep the event_version column as `Version` (no
ingest.go:303 overwrite), so within an archive source the winner is max
event_version — the cache's first key. The same uid in two archive files does
not occur (one row per uid in ClickHouse, reruns rewrite); Path makes it
deterministic if it ever does. Overlapping sessions land in one component via
shared ckpt session ids (ingest.go:780-810), so winners see both copies;
losers still feed the session authors section (authors.go:13-31), which is the
cache's union of file author and path author (index_session_authors.go:28-37)
— the same literal for archive rows by construction (lake.go:583,639).

### (d) Parts, components, winners — no parser, no Version

The archive ingest branch sits before `normalize.For` in `ingestSource`
(ingest.go:253-256), since archive harnesses do have parsers: `ingestArchive`
opens the parquet (no manifest — no `src.kind.open`), maps rows through
`rowFrom`, and streams them into the per-session members' light/heavy frame
logs (ingest.go:294-318) — one pass over the file, whichever session a row
belongs to. Member ckpts record `Parser: 0`, `Layout: logLayout` (the row
encoding is shared), and the parser-version machinery skips the kind by
construction: `versionStale` is false for any kind but KindStore
(ingest.go:584-589), `parserVersion` is never reached for it, and the kind's
own byte test replaces `extends` (a parquet has no parse-resume state; the
whole file re-parses into the members' next gen). The file ckpt is the commit
point: it is written last, after every member's logs are synced and member
ckpts renamed, so it never names a member whose rows are not durable, and it
names the sessions the file held so a session the file lost has its member
ckpt and logs removed (archive.go `removeMember`).

`publish` expands the file ckpt into its member ckpts and plans those; the
file itself never joins planning (its ckpt is kept from gc by
`gcLogs`' keepExtra). A member is hot exactly like a raw source
(`ColdZst` set at its first settle), shards into the 256 cold parts by least
session id once settled, and joins a raw source's hot component as a cold
member when they share a session — so a hot part holds one session's rows of
raw + archive, never the file's (the committed test asserts the literal
count). Sections (spans, annotations, authors) are functions of the part's
rows (DESIGN.md:114-117) and build from winner rows as for any source.

### (e) Tenancy, authors, controls

`Author` is the path author, via the ckpt (ingest.go:217) into
`Row.Author` (build.go:87) and `buildSessionAuthors` (authors.go:13-31) — the
same derivation the cache makes from the lake path
(index_session_authors.go:19-24); the file's author column is the same value
by construction (selectList stamps the import author, lake.go:583,633-644) and
is not read. No machine: ch rows' machine_id is the "archive-ch" literal
(lake.go:639), and no engine row carries a machine. Controls are session-id
records through `Controls.record` with identity from ctx (controls.go:212),
never from rows; archive sessions are ordinary session ids to scope checks
(scope.go:17,:27). One publish hook: archive sources are NOT appended to
CURRENT's `sources` rows (ingest.go:599-614) — the cache's `sources` table
skips archive files (index.go:814-816), so get_ingest_status and list_sessions
must not count them as sources either; their rows still count in parts and
ls rollups. GC is unaffected: the ckpt and logs of every member are kept
whatever the sources rows say (ingest.go:846-863).

### (f) Memory

`parquet.OpenFile` reads footer + metadata only; rows stream one row group at
a time into a bounded slice (`archiveReadRows`), so peak memory is one
decompressed group's columns plus the slice — never a 6 GB file, never an
mmap. A file's rows are not sorted by session, so the per-session members'
frame logs are held behind an LRU of `archiveOpenLogs` open pairs: the least
recently used member is flushed and closed, and reopens at its recorded
lengths (same gen — the gen bump is per parse run, not per open) if a later
row needs it, so ~2,800 sessions cost 64 open files, and the committed test's
70-session file exercises the reopen. One archive source per worker, like
today's parse (ingest.go:62-64). The engine never writes these files; no
writer exists outside tests.

### (g) Fixture

The probe (scratch, gitignored, run under heavy.lock) extracted 59796bc to a
mktemp dir, synthesized a 2-row export parquet with DuckDB (cgo build of
59796bc itself) and ran `lake.ImportClickHouse`, then decoded the resulting
ch-schema file with parquet-go CGO_ENABLED=0: 2 rows, 62 leaf columns, 2 list
elements, 2+2 map keys, correct event_ts micros and session_date days —
counts only. Phase 2's committed `archive_test.go` writes its small ch-schema
fixture with parquet-go's writer inside the test (no writer in non-test code)
and asserts, with literal values: per-session member ckpts carrying the
parent file's identity; the raw copy winning the shared uid; archive-only
sessions listed and searched; session authors = path author; get_ingest_status
counting one source, not the members; an unchanged re-ingest rewriting
nothing (store tree digest equal); a changed file re-parsing all members in
one pass; the append bound (the hot part holds 13 docs — the session's — not
the file's rows); and a 70-session file driving the LRU, including a reopen.
