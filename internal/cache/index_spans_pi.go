package cache

func init() {
	RegisterSpans("pi-coding-agent", flatSpans("pi-coding-agent", piFamilyChatKey))
}

// piFamilyChatKey: every block of an assistant message carries the record id
// in item_id and the usage rides on one of them, so the id names the chat.
const piFamilyChatKey = `CASE WHEN actor_kind = 'assistant' AND event_kind IN ('message', 'reasoning', 'tool_call') THEN item_id END`
