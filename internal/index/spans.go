package index

import "github.com/candyflipline/mnemonic/internal/schema"

// buildSpans is a part's spans section for one session: the spans
// internal/cache/index_spans_<harness>.go derives from the session's winner
// rows of that harness, in event order with turn_seq current.
func buildSpans(harness string, session []*Row) []schema.Span { panic("lane U5-spans") }

// sessionSpans is sessionID's spans, ordered by start then span_id.
func (db *DB) sessionSpans(sessionID string) []schema.Span { panic("lane U5-spans") }
