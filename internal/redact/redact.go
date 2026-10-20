// Package redact removes secrets from raw lines on the client, before they
// leave the machine. Port of upstream crates/moraine-ingest-core/src/redaction.rs
// (26 rules plus the keyword prefilter). Owned by lane redact.
package redact

// Redactor is safe for concurrent use.
type Redactor struct{}

// New compiles the built-in rules plus extra regex patterns from config.
func New(extraPatterns []string) (*Redactor, error) { panic("lane redact") }

// Line returns line with every secret replaced exactly as upstream does. A
// replacement never introduces '"', '\\' or '\n', so a JSON line stays valid.
func (r *Redactor) Line(line []byte) []byte { panic("lane redact") }
