package otlp

import "github.com/candyflipline/mnemonic/internal/schema"

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

// Gate names attributes dropped before anything is stored unless the server
// sets otlp.store_content. Prefix gates also match name + ".". In limits the
// gate to records (spans, span events, log events) whose name, without a
// claude_code. or codex. prefix, is listed; Name "*" drops every attribute of
// such a record but event.name.
type Gate struct {
	Name   string
	Prefix bool
	In     []string
	Source string
}

var Gates = []Gate{
	{Name: "gen_ai.input.messages", Source: "otel-genai: message content"},
	{Name: "gen_ai.output.messages", Source: "otel-genai: message content"},
	{Name: "gen_ai.system_instructions", Source: "otel-genai: system prompt"},
	{Name: "gen_ai.tool.call.arguments", Source: "otel-genai: tool input"},
	{Name: "gen_ai.tool.call.result", Source: "otel-genai: tool output"},
	{Name: "gen_ai.tool.definitions", Source: "otel-genai: opt-in tool schemas"},
	{Name: "gen_ai.prompt", Prefix: true, Source: "openllmetry: flattened prompts"},
	{Name: "gen_ai.completion", Prefix: true, Source: "openllmetry: flattened completions"},
	{Name: "input.value", Source: "openinference: span input"},
	{Name: "output.value", Source: "openinference: span output"},
	{Name: "llm.input_messages", Prefix: true, Source: "openinference: messages"},
	{Name: "llm.output_messages", Prefix: true, Source: "openinference: messages"},
	{Name: "llm.prompts", Prefix: true, Source: "openinference: prompts"},
	{Name: "llm.prompt_template", Prefix: true, Source: "openinference: template and variables"},
	{Name: "tool.parameters", Source: "openinference: tool input"},
	{Name: "retrieval.documents", Prefix: true, Source: "openinference: retrieved text"},
	{Name: "embedding.embeddings", Prefix: true, Source: "openinference: embedded text"},
	{Name: "reranker.input_documents", Prefix: true, Source: "openinference: document text"},
	{Name: "reranker.output_documents", Prefix: true, Source: "openinference: document text"},
	{Name: "process.command_line", Source: "otel resource: the command line can carry a prompt"},
	{Name: "process.command_args", Source: "otel resource: the command line can carry a prompt"},

	{Name: "prompt", In: []string{"user_prompt"}, Source: "claude-code, codex: prompt text"},
	{Name: "prompt_text", In: []string{"user_prompt"}, Source: "claude-code: prompt text"},
	{Name: "user_prompt", In: []string{"interaction"}, Source: "claude-code: prompt text"},
	{Name: "new_context", In: []string{"interaction", "llm_request", "tool"}, Source: "claude-code: detailed tracing content"},
	{Name: "system_reminders", In: []string{"llm_request"}, Source: "claude-code: detailed tracing content"},
	{Name: "system_prompt_preview", In: []string{"llm_request"}, Source: "claude-code: system prompt"},
	{Name: "user_system_prompt", In: []string{"llm_request"}, Source: "claude-code: system prompt"},
	{Name: "response.model_output", In: []string{"llm_request"}, Source: "claude-code: model text"},
	{Name: "response", In: []string{"assistant_response"}, Source: "claude-code: model text"},
	{Name: "body", In: []string{"api_request_body", "api_response_body"}, Source: "claude-code: raw API JSON"},
	{Name: "*", In: []string{"system_prompt", "tool.output"}, Source: "claude-code: system prompt, tool output"},
	{Name: "tool_input", In: []string{"tool_result", "tool"}, Source: "claude-code: tool arguments"},
	{Name: "tool_parameters", In: []string{"tool_result", "tool_decision"}, Source: "claude-code: tool arguments"},
	{Name: "full_command", In: []string{"tool"}, Source: "claude-code: tool detail"},
	{Name: "file_path", In: []string{"tool"}, Source: "claude-code: tool detail"},
	{Name: "error", In: []string{"tool_result", "tool.execution"}, Source: "claude-code: full error text"},
	{Name: "hook_definitions", In: []string{"hook"}, Source: "claude-code: tool detail"},
	{Name: "arguments", In: []string{"tool_result"}, Source: "codex: tool arguments"},
	{Name: "output", In: []string{"tool_result"}, Source: "codex: tool output snippet"},
}
