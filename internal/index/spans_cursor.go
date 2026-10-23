package index

import "github.com/candyflipline/mnemonic/internal/schema"

// cursorSpans: flatSpans with no chat key, a cursor session records no model
// call boundary; every execute_tool hangs under its turn.
func cursorSpans(harness string, session []*Row) []schema.Span {
	return flatSpans("cursor", nil)(harness, session)
}
