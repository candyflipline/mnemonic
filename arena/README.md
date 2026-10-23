# arena — the parity harness

The oracle is main's DuckDB path (`lake.Run` → `cache.Converge` → `query.Lookup`,
wrapped by `arena/duck`). A candidate binary (the engine) must answer the same
tool calls with the same bytes. `check` scores a candidate against the oracle's
expected answers over an arena fixture dir.

## Build

From the repo root (oracle and gen link DuckDB, so cgo on; check and seed are pure Go):

```
CGO_ENABLED=1 go build -o /tmp/arena-bin/oracle ./arena/oracle
CGO_ENABLED=1 go build -o /tmp/arena-bin/gen ./arena/gen
CGO_ENABLED=0 go build -o /tmp/arena-bin/check ./arena/check
CGO_ENABLED=0 go build -o /tmp/arena-bin/seed ./arena/seed
```

## Fixture dir

```
<arena>/
  raw/                      the corpus (author=*/... sources)
  golden/calls.jsonl        63 calls with ids
  golden/expected.jsonl     the oracle's answers, one per call, same order
  hidden/calls.jsonl        63 more calls (never shown to a candidate author)
  hidden/expected.jsonl
  check/masks.json          per call, the paths that may vary between two runs
  runs/                     check writes its result JSON here
```

The oracle's expected answers are main 59796bc's `internal/query` `Call` output.

## The line protocol

A candidate is any binary speaking two commands:

```
<bin> ingest -root RAWROOT -store STORE
<bin> batch -root RAWROOT -store STORE < calls.jsonl > results.jsonl
```

`ingest` brings the store under `-store` up to date with everything under
`RAWROOT/raw`; it may be called repeatedly as raw appends. `batch` reads one
call per stdin line and writes one reply per stdout line:

```
{"id":7,"tool":"search_sessions","args":{"query":"...","limit":10}}
{"id":7,"data":{...},"text":"...","error":"","us":1234}
```

`args` are exactly the arguments of `internal/mcp/tools.json` for that tool.
`data` and `text` are what `internal/query`'s `Call` returns; `error` is empty
on success; `us` is server-side microseconds (never compared). An empty or
blank line is skipped. The reply's `id` echoes the call's `id` verbatim.

## Write tools

`annotate`, `set_visibility` and `delete_session` write through the request
context's store, exactly as the server does (`internal/server` puts identity,
`store.WithStore` and `query.WithControls` on each request ctx). A write lands
as a new raw source under `STORE/raw` (`harness=annotation` or
`harness=control`), so the store's raw stays the source of truth; the effect
becomes readable after the next `ingest` parses and converges that source —
the same cycle a server client observes after a PUT. A call sequence for the
e2e lane is therefore expressed as:

1. `batch` with the write calls (annotate / set_visibility / delete_session),
2. `ingest -root RAWROOT -store STORE`,
3. `batch` with the read calls that must observe the writes.

The oracle acts as one fixed author: `-author` if given, else the first
`author=*` dir under the data root's raw, else `arena`.

## Scoring a candidate

```
/tmp/arena-bin/check -bin <candidate binary> -name <name> \
  -arena /path/to/arena-fixture [-src /path/to/transcripts] \
  [-skip incremental,crash,appendcrash]
```

Phases, each answered over the full golden+hidden call sets and compared
against `expected.jsonl` under `masks.json`:

- clean (always): one `ingest` of the fixture raw into a fresh store, one `batch`.
- incremental: `seed -src SRC -half` a fresh raw root, `ingest`, `seed -rest`
  (appends new transcripts), `ingest` again, `batch`. `-src` defaults to
  `<arena>/../sample2`; point it at the transcript dir the fixture's README
  names (60 claude + 10 codex sources).
- crash: three SIGKILLs mid-`ingest` into one store, then a final `ingest`;
  answers must match.
- appendcrash: the same with appends between kills.

`-skip` takes a comma list of phase names. The result JSON lands in
`<arena>/runs/<name>-<stamp>.json` (`pass`, per-tool `pass`/`fail` counts, up
to three first diffs per tool with the JSON paths that differed).

Example, the oracle scored against itself (must be N/N, pass=true):

```
/tmp/arena-bin/check -bin /tmp/arena-bin/oracle -name oracle-self \
  -arena /path/to/arena-fixture -skip incremental,crash,appendcrash
```

## Regenerating expected answers

```
CGO_ENABLED=1 /tmp/arena-bin/oracle build -raw <fixture raw root> -root <fresh oracle root>
/tmp/arena-bin/gen -root <oracle root> -oracle /tmp/arena-bin/oracle -seed 1 -out <arena>
```

`gen` redraws calls from the oracle's index and writes `calls.jsonl` plus, via
`oracle answer -root <oracle root>`, `expected.jsonl` into `-out`. To keep the
calls and recompute only the answers, run `oracle answer` over the existing
calls and write the lines back (that is how the 59796bc fixture was made).
