// Package claudecode parses claude-code sources. Port of upstream
// crates/moraine-ingest-core/src/sources/claude_code.rs plus the shared helpers it
// calls; testdata/goldens pins the output. Owned by lane parser-claudecode, which
// adds `func init() { normalize.Register(Parser{}) }` once the golden is green.
package claudecode
