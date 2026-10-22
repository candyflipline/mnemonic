package otlp

import (
	"encoding/json"

	"github.com/candyflipline/mnemonic/internal/schema"
)

const (
	ServiceName           = "service.name"
	EventNameKey          = "event.name"
	OperationName         = "gen_ai.operation.name"
	OpenInferenceSpanKind = "openinference.span.kind"
	CostUnit              = "cost_usd"
)

var (
	SessionKeys   = []string{"gen_ai.conversation.id", "session.id", "conversation.id"}
	ModelKeys     = []string{"gen_ai.response.model", "gen_ai.request.model", "llm.model_name", "model"}
	ProviderKeys  = []string{"gen_ai.provider.name", "gen_ai.system", "llm.provider", "llm.system"}
	ResponseIDs   = []string{"gen_ai.response.id", "request_id"}
	ToolNameKeys  = []string{"gen_ai.tool.name", "tool.name", "tool_name"}
	ToolCallIDs   = []string{"gen_ai.tool.call.id", "tool.id", "tool_use_id", "call_id"}
	AgentNameKeys = []string{"gen_ai.agent.name", "agent.name"}
	AgentIDKeys   = []string{"gen_ai.agent.id", "agent_id"}

	InputTokenKeys      = []string{"gen_ai.usage.input_tokens", "llm.token_count.prompt", "input_tokens", "input_token_count"}
	OutputTokenKeys     = []string{"gen_ai.usage.output_tokens", "llm.token_count.completion", "output_tokens", "output_token_count"}
	CacheReadTokenKeys  = []string{"gen_ai.usage.cache_read.input_tokens", "llm.token_count.prompt_details.cache_read", "cache_read_tokens", "cached_token_count"}
	CacheWriteTokenKeys = []string{"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.cache_creation.input_tokens", "llm.token_count.prompt_details.cache_write", "cache_creation_tokens", "cache_write_token_count"}
	CostKeys            = []string{"gen_ai.usage.cost", "cost_usd"}
)

var operationKinds = map[string]schema.SpanKind{
	"chat":             schema.SpanChat,
	"text_completion":  schema.SpanChat,
	"generate_content": schema.SpanChat,
	"execute_tool":     schema.SpanExecuteTool,
	"invoke_agent":     schema.SpanInvokeAgent,
	"create_agent":     schema.SpanInvokeAgent,
}

var openInferenceKinds = map[string]schema.SpanKind{
	"LLM":   schema.SpanChat,
	"TOOL":  schema.SpanExecuteTool,
	"AGENT": schema.SpanInvokeAgent,
}

var spanNameKinds = map[string]schema.SpanKind{
	"claude_code.llm_request": schema.SpanChat,
	"claude_code.tool":        schema.SpanExecuteTool,
	"claude_code.interaction": schema.SpanInvokeAgent,
}

// KindOf is the span kind a stored span stands for, "" when it is none of
// the kinds the spans relation holds. gen_ai.operation.name wins over
// openinference.span.kind, which wins over the span's name.
func KindOf(r *Record) schema.SpanKind {
	if k, ok := operationKinds[Str(r.Attributes, OperationName)]; ok {
		return k
	}
	if k, ok := openInferenceKinds[Str(r.Attributes, OpenInferenceSpanKind)]; ok {
		return k
	}
	return spanNameKinds[r.Name]
}

// allowed is every attribute key kept when the server does not set
// otlp.store_content: the keys the parser and the span derivation read, which
// are the key lists above, and the few resource, scope and error-type keys
// below. Any other key, whatever its name suggests, is dropped from the
// resource, the span, its events and the log record before anything is stored.
var allowed = keySet(
	[]string{
		ServiceName, EventNameKey, OperationName, OpenInferenceSpanKind, "duration_ms", "exception.type",
		"service.version", "host.name", "telemetry.sdk.name", "telemetry.sdk.version", "telemetry.sdk.language",
	},
	SessionKeys, ModelKeys, ProviderKeys, ResponseIDs, ToolNameKeys, ToolCallIDs, AgentNameKeys, AgentIDKeys,
	InputTokenKeys, OutputTokenKeys, CacheReadTokenKeys, CacheWriteTokenKeys, CostKeys,
)

func keySet(lists ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, l := range lists {
		for _, k := range l {
			m[k] = true
		}
	}
	return m
}

// gate keeps the scalar values of the allowed keys of attrs and deletes the
// rest, so an array or a key-value list never survives under any key.
func gate(attrs map[string]any) {
	for k, v := range attrs {
		switch v.(type) {
		case string, bool, json.Number, float64:
			if allowed[k] {
				continue
			}
		}
		delete(attrs, k)
	}
}
