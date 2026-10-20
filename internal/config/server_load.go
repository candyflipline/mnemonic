package config

import (
	"fmt"
	"regexp"

	"github.com/BurntSushi/toml"
)

// sha256Hex is what auth.Hash writes into the file: 64 lowercase hex digits.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LoadServer reads the server file and rejects an empty data_dir or listen,
// no tokens, a token with an empty author or a non-sha256 hash, or one hash
// mapped to two authors.
func LoadServer(path string) (Server, error) {
	var cfg Server
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Server{}, fmt.Errorf("read server config: %w", err)
	}
	if cfg.DataDir == "" {
		return Server{}, fmt.Errorf("%s: data_dir must be set", path)
	}
	if cfg.Listen == "" {
		return Server{}, fmt.Errorf("%s: listen must be set", path)
	}
	if len(cfg.Tokens) == 0 {
		return Server{}, fmt.Errorf("%s: at least one [[tokens]] entry is required", path)
	}
	seen := map[string]string{}
	for i, t := range cfg.Tokens {
		if t.Author == "" {
			return Server{}, fmt.Errorf("%s: tokens[%d]: author must be set", path, i)
		}
		if !sha256Hex.MatchString(t.SHA256) {
			return Server{}, fmt.Errorf("%s: tokens[%d] (%s): sha256 must be 64 lowercase hex digits", path, i, t.Author)
		}
		if prev, dup := seen[t.SHA256]; dup {
			return Server{}, fmt.Errorf("%s: tokens[%d]: sha256 already used by %s", path, i, prev)
		}
		seen[t.SHA256] = t.Author
	}
	return cfg, nil
}
