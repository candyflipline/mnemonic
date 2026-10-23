# ql: the query language of the `query` tool

The `query` tool answers one read-only question over five relations (events,
sessions, turns, spans, annotations). Until now the question was SQL run by
DuckDB behind a guard (`internal/query/sqltool.go`, `internal/query/sqlguard.go`).
The owner's call (Oct 23): keep the tool, drop DuckDB and the SQL structure,
make it Braintrust BTQL-shaped. `internal/ql` is that language: a grammar, a
typed AST parsed once at the tool boundary, a checker against the schema
(relations and columns as data), and an evaluator over a small row-source
interface the engine implements.

## Lies table

| Lie underneath | The one fix | Deleted |
|---|---|---|
| `query` means SQL, so it needs a SQL engine | a clause language with no statements, no table names beyond five, no subqueries, parsed once into a typed AST and run over engine rows | the SQL surface, `sqlguard.go` and its 400-name function allowlist, DuckDB's parser as a security boundary |
| Safety comes from walking an attacker's syntax tree | nothing unsafe is expressible: the grammar has no statement other than a query, `from:` takes one of five names, functions are a closed list of eleven | every refusal test that guarded a SQL escape (they become parse errors by construction) |
| A filter must decode the whole row | rows are lazy: the evaluator asks for a column only when a predicate needs it, cheap conjuncts first, heavy columns (payload_json, token_usage_json) last | full-row materialization per event |
| Full text needs a scan | `match` is an AST node the checker lifts into the scan request, so the source can drive it from postings; the evaluator re-checks every row | nothing (the recheck keeps a source that ignores the hint correct) |

## Candidates

The same agent question in each: "codex tool calls per tool in February, most
first, top 10".

**A. BTQL clauses.** One clause per concern, each named, in any order, separated
by `|` or a newline.

    from: events
    | filter: harness = 'codex' and tool_phase = 'request'
    | dimensions: tool_name
    | measures: count() as calls
    | sort: calls desc
    | limit: 10

**B. A JSON object.** The tool's `query` argument is an object; the expression
language is nested arrays.

    {"from": "events",
     "filter": {"and": [["=", "harness", "codex"], ["=", "tool_phase", "request"]]},
     "dimensions": ["tool_name"],
     "measures": [{"count": [], "as": "calls"}],
     "sort": [["calls", "desc"]], "limit": 10}

**C. A pipeline (KQL / PRQL style).** Stages applied left to right; each stage
reads the previous one's output.

    events
    | where harness == 'codex' and tool_phase == 'request'
    | summarize calls = count() by tool_name
    | sort by calls desc
    | take 10

| | A. BTQL clauses | B. JSON object | C. Pipeline |
|---|---|---|---|
| What an agent writes | short, close to the SQL it knows; BTQL is documented publicly | 2-3x longer; easy to nest a filter wrong; JSON schema can describe the clause keys but not the expressions | short; agents know KQL less well than SQL; `==` vs `=` trips them |
| Parser | ~600 lines, hand-written recursive descent | none for clauses (encoding/json), but an expression decoder over `[]any` that must refuse every shape by hand: the same work, worse errors (no line:col) | as A, plus stage typing: each stage's output schema is the next stage's input |
| Semantics | fixed order: from, filter, group, sort, limit; one shape to evaluate | as A | open-ended: filter after summarize, two summarizes, sort before filter; the evaluator becomes a general relational engine |
| Errors | `line:col: message` on the source text | a JSON path, no position in what the agent wrote | as A |
| Cost bound | known before the scan: at most one grouping, one sort, a top-k heap | as A | each stage needs its own bound; a summarize over a summarize holds two group tables |
| Pushdown | filter is one expression over one relation: conjuncts, `match` hints and needed columns fall out of the AST | as A | only stages before the first summarize push down; the rest is in memory |
| SQL leakage | none: no statement keyword, no `FROM t1, t2`, no subquery | none | none |

**Pick: A.** It is what the owner named (Braintrust BTQL), it is the shortest
thing an agent can write correctly on the first try, and its fixed clause order
makes the evaluator one pass with a bounded state (filter, then one group table
or one top-k heap, then the cut). B moves the parse into hand-checked `[]any`
shapes with worse errors and longer queries for no safety gain, because A has
no unsafe construct either. C buys composability (filter after aggregate, two
aggregation levels) that no test in `sqltool_test.go` and no listed agent
question needs, and pays for it with an evaluator that is a general relational
engine. If having-style filters are ever needed, BTQL's own answer is one more
clause (`final_filter:`), not a pipeline.
