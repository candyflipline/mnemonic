package index

import (
	"iter"

	"github.com/candyflipline/mnemonic/internal/schema"
)

func primeAgentSpans(harness, sid string, rows iter.Seq[*borrowedRow]) []schema.Span {
	return flatSpans("prime-agent", piFamilyChatKey)(harness, sid, rows)
}
