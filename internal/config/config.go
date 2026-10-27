package config

import "github.com/candyflipline/mnemonic/internal/auth"

// Client is ~/.mnemonic/config.toml. Server is quic://host:port, the only
// scheme.
//
//	server     = "quic://mnemonic.example:8741"
//	server_key = "..."                # the server's key: `mnemonic key --file` on the server
//	key_file   = "~/.mnemonic/key"    # this machine's key; this is the default
//	machine    = "chris-mbp"          # defaults to the short hostname
//	[[sources]]
//	harness = "claude-code"; source_name = "claude"; glob = "~/.claude/projects/**/*.jsonl"
//	[redaction]
//	extra_patterns = []
type Client struct {
	Server    string          `toml:"server"`
	ServerKey string          `toml:"server_key"`
	KeyFile   string          `toml:"key_file"`
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
//	[quic]
//	listen   = ":8741"                  # UDP
//	key_file = "/var/lib/mnemonic/server.key"  # optional; default data_dir/server.key
//	[[keys]]
//	key = "<mnemonic key prints this>"; author = "chris"
//	tenant = "acme"            # optional, default "default"; an author sits in one tenant
//	scopes = ["read", "push"]  # optional, the default; "annotate" and "admin" opt in
//	[otlp]
//	store_content = false    # keep OTLP message and tool content
//	[share]                  # optional; without it no link is served
//	listen    = "127.0.0.1:8742"            # loopback only; the tunnel publishes it
//	base_url  = "https://share.askloyal.com"
//	quic_addr = "mnemonic.askloyal.com:8741" # what a joining machine dials
//
// An author no key entry names is in the "default" tenant, so every
// default-tenant key reads what such an author's data holds.
type Server struct {
	DataDir string     `toml:"data_dir"`
	QUIC    QUIC       `toml:"quic"`
	Keys    []KeyEntry `toml:"keys"`
	OTLP    OTLP       `toml:"otlp"`
	Share   *Share     `toml:"share"`
}

type Share struct {
	Listen   string `toml:"listen"`
	BaseURL  string `toml:"base_url"`
	QUICAddr string `toml:"quic_addr"`
}

const DefaultShareListen = "127.0.0.1:8742"

type QUIC struct {
	Listen  string `toml:"listen"`
	KeyFile string `toml:"key_file"`
}

type KeyEntry struct {
	Key    string       `toml:"key"`
	Author string       `toml:"author"`
	Tenant string       `toml:"tenant"`
	Scopes []auth.Scope `toml:"scopes"`
}

type OTLP struct {
	StoreContent bool `toml:"store_content"`
}
