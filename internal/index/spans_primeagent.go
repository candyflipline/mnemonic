package index

import "github.com/candyflipline/mnemonic/internal/schema"

// primeAgentSpans: flatSpans over piFamilyChatKey, the prime-agent record
// shape pi-coding-agent shares.
func primeAgentSpans(harness string, session []*Row) []schema.Span {
	return flatSpans("prime-agent", piFamilyChatKey)(harness, session)
}
