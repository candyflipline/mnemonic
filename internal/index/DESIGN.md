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
| U10 | `internal/mcp/mcp.go` | `redactResult(out *redact.Redactor, result any) any` | every string in a tools/call result through `out.Line`; called in `Handler`'s tools/call branch when `out != nil` | redactor goldens |
| U10 | `cmd/mnemonic/local.go` | `runLocal(ctx, opts, out, logOut) error` | in-place index, push.Watch events call ingest, MCP with `Handler(ix, index.Lookup, red)` | standing predicate 7 |

The five old tools are ported with main's Oct 22 scope (`scope.go`); there
are no U4 stubs. U4 lanes own their tool's parity diffs (see the U0 report).
