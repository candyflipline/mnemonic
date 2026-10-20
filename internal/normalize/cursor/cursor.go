// Package cursor parses cursor sources. Port of upstream
// crates/moraine-ingest-core/src/sources/cursor.rs plus the shared helpers it
// calls; testdata/goldens pins the output. Owned by lane parser-cursor, which
// adds `func init() { normalize.Register(Parser{}) }` once the golden is green.
package cursor
