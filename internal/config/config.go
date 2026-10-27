// Package config reads the two TOML files: ~/.mnemonic/config.toml on each
// client and the server's config. Validation happens here, once.
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
//	listen   = ":443"
//	push_listen = ":8741"
//	[tls]                    # omit for plaintext listeners
//	hosts = ["mnemonic.example"]
//	acme_directory = "https://acme-staging-v02.api.letsencrypt.org/directory"  # optional; default Let's Encrypt production
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
	TLS        TLS          `toml:"tls"`
	Tokens     []TokenEntry `toml:"tokens"`
	QUIC       QUIC         `toml:"quic"`
	Keys       []KeyEntry   `toml:"keys"`
	OTLP       OTLP         `toml:"otlp"`
}

// QUIC is the one UDP endpoint. KeyFile holds the server's ed25519 key and
// defaults to data_dir/server.key.
type QUIC struct {
	Listen  string `toml:"listen"`
	KeyFile string `toml:"key_file"`
}

// KeyEntry admits one client key (auth.EncodeKey form) as Author.
type KeyEntry struct {
	Key    string       `toml:"key"`
	Author string       `toml:"author"`
	Tenant string       `toml:"tenant"`
	Scopes []auth.Scope `toml:"scopes"`
}

// TLS serves both listeners with Let's Encrypt certificates for Hosts,
// cached under data_dir/autocert.
type TLS struct {
	Hosts         []string `toml:"hosts"`
	ACMEDirectory string   `toml:"acme_directory"`
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
