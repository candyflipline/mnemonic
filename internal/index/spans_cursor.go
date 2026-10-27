package index

import (
	"iter"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// cursorSpans: flatSpans with no chat key, a cursor session records no model
// call boundary; every execute_tool hangs under its turn.
func cursorSpans(harness, sid string, rows iter.Seq[*borrowedRow]) []schema.Span {
	return flatSpans("cursor", nil)(harness, sid, rows)
}
