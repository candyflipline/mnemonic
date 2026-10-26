// Package config reads the two TOML files: ~/.mnemonic/config.toml on each
// client and the server's config. Validation happens here, once.
package config

import "github.com/candyflipline/mnemonic/internal/auth"

// Client is ~/.mnemonic/config.toml.
//
//	server  = "https://mnemonic.example"
//	token   = "..."            # or token_file = "~/.mnemonic/token"
//	machine = "chris-mbp"      # defaults to the short hostname
//	[[sources]]
//	harness = "claude-code"; source_name = "claude"; glob = "~/.claude/projects/**/*.jsonl"
//	[redaction]
//	extra_patterns = []
type Client struct {
	Server    string          `toml:"server"`
	Token     string          `toml:"token"`
	TokenFile string          `toml:"token_file"`
	Machine   string          `toml:"machine"`
	Sources   []SourceGlob    `toml:"sources"`
	Redaction ClientRedaction `toml:"redaction"`
}

type SourceGlob struct {
	Harness    string `toml:"harness"`
	SourceName string `toml:"source_name"`
	Glob       string `toml:"glob"` // ~ expanded; ** matches any depth
}

type ClientRedaction struct {
	ExtraPatterns []string `toml:"extra_patterns"`
}

var DefaultSources = []SourceGlob{
	{"claude-code", "claude", "~/.claude/projects/**/*.jsonl"},
	{"claude-code", "claude-glm", "~/.claude-glm/projects/**/*.jsonl"},
	{"codex", "codex", "~/.codex/sessions/**/*.jsonl"},
	{"cursor", "cursor", "~/.cursor/projects/*/agent-transcripts/**/*.jsonl"},
	{"claude-code", "claude-cowork", "~/Library/Application Support/Claude/local-agent-mode-sessions/**/.claude/projects/**/*.jsonl"},
	{"kiro-cli", "kiro", "~/.kiro/sessions/cli/*.jsonl"},
	{"kimi-cli", "kimi", "~/.kimi/sessions/**/wire.jsonl"},
	{"qwen-code", "qwen", "~/.qwen/projects/*/chats/*.jsonl"},
	{"pi-coding-agent", "pi", "~/.pi/agent/sessions/**/*.jsonl"},
	{"pi-coding-agent", "omp", "~/.omp/agent/sessions/**/*.jsonl"},
	{"prime-agent", "prime-agent", "~/.prime/agent/sessions/*.jsonl"},
	{"prime-agent", "prime-agent-subagents", "~/.prime/agent/session-artifacts/**/sub-*/*.jsonl"},
}

// Server is the server's config file.
//
//	data_dir = "/var/lib/mnemonic"
//	listen   = "0.0.0.0:8740"
//	push_listen = "0.0.0.0:8741"
//	[[tokens]]
//	author = "chris"; sha256 = "<auth.Hash(token)>"
//	tenant = "acme"            # optional, default "default"; an author sits in one tenant
//	scopes = ["read", "push"]  # optional, the default; "annotate" and "admin" opt in
//	[otlp]
//	store_content = false    # keep OTLP message and tool content
//
// An author no token entry names is in the "default" tenant, so every
// default-tenant token reads what such an author's data holds.
type Server struct {
	DataDir    string       `toml:"data_dir"`
	Listen     string       `toml:"listen"`
	PushListen string       `toml:"push_listen"`
	Tokens     []TokenEntry `toml:"tokens"`
	OTLP       OTLP         `toml:"otlp"`
}

type OTLP struct {
	StoreContent bool `toml:"store_content"`
}

type TokenEntry struct {
	Author string       `toml:"author"`
	SHA256 string       `toml:"sha256"`
	Tenant string       `toml:"tenant"`
	Scopes []auth.Scope `toml:"scopes"`
}
