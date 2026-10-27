package index

import (
	"iter"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// piFamilyChatKey: every block of an assistant message carries the record id
// in item_id and the usage rides on one of them, so the id names the chat.
func piFamilyChatKey(r *Row) string {
	if r.ActorKind == "assistant" {
		switch r.EventKind {
		case "message", "reasoning", "tool_call":
			return r.ItemID
		}
	}
	return ""
}

func piSpans(harness, sid string, rows iter.Seq[*Row]) []schema.Span {
	return flatSpans("pi-coding-agent", piFamilyChatKey)(harness, sid, rows)
}
