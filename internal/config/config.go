// Package config reads the two TOML files: ~/.mnemonic/config.toml on each
// client and the server's config. Validation happens here, once.
package config

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

// DefaultSources are the upstream defaults
// (upstream config/src/lib.rs:764-873 at upstream/main), used when the
// file lists none. A harness without a parser is still pushed and kept raw.
var DefaultSources = []SourceGlob{
	{"claude-code", "claude", "~/.claude/projects/**/*.jsonl"},
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
//	listen   = "127.0.0.1:8740"
//	[[tokens]]
//	author = "chris"; sha256 = "<auth.Hash(token)>"
type Server struct {
	DataDir string       `toml:"data_dir"`
	Listen  string       `toml:"listen"`
	Tokens  []TokenEntry `toml:"tokens"`
}

type TokenEntry struct {
	Author string `toml:"author"`
	SHA256 string `toml:"sha256"`
}
