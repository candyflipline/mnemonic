package index

import (
	"iter"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// primeAgentSpans: flatSpans over piFamilyChatKey, the prime-agent record
// shape pi-coding-agent shares.
func primeAgentSpans(harness, sid string, rows iter.Seq[*Row]) []schema.Span {
	return flatSpans("prime-agent", piFamilyChatKey)(harness, sid, rows)
}
