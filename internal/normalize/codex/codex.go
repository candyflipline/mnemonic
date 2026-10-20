// Package codex parses codex sources. Port of upstream
// crates/moraine-ingest-core/src/sources/codex.rs plus the shared helpers it
// calls; testdata/goldens pins the output. Owned by lane parser-codex, which
// adds `func init() { normalize.Register(Parser{}) }` once the golden is green.
package codex
