package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// machineRe is what the server accepts for ?machine=; validating here means a
// bad hostname fails at load, not on the wire.
var machineRe = regexp.MustCompile(`^[a-z0-9._-]+$`)

// LoadClient reads the client file, expands ~, fills Machine from the short
// hostname and Sources from DefaultSources when empty, reads TokenFile, and
// rejects a missing server or token. Owned by lane push.
func LoadClient(path string) (Client, error) {
	path, err := expandTilde(path)
	if err != nil {
		return Client{}, err
	}
	var cfg Client
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Client{}, fmt.Errorf("config: %w", err)
	}
	if cfg.Server == "" {
		return Client{}, fmt.Errorf("config: %s: server is required", path)
	}
	if cfg.Token == "" && cfg.TokenFile != "" {
		tokenFile, err := expandTilde(cfg.TokenFile)
		if err != nil {
			return Client{}, err
		}
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			return Client{}, fmt.Errorf("config: token_file: %w", err)
		}
		cfg.Token = strings.TrimSpace(string(raw))
	}
	if cfg.Token == "" {
		return Client{}, fmt.Errorf("config: %s: token or token_file is required", path)
	}
	if cfg.Machine == "" {
		host, err := os.Hostname()
		if err != nil {
			return Client{}, fmt.Errorf("config: machine: %w", err)
		}
		cfg.Machine = shortHost(host)
	}
	if !machineRe.MatchString(cfg.Machine) {
		return Client{}, fmt.Errorf("config: machine %q must match [a-z0-9._-]+", cfg.Machine)
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = append([]SourceGlob(nil), DefaultSources...)
	}
	for i, src := range cfg.Sources {
		glob, err := expandTilde(src.Glob)
		if err != nil {
			return Client{}, err
		}
		cfg.Sources[i].Glob = glob
	}
	return cfg, nil
}

// expandTilde rewrites a leading ~ with the home directory, leaving the rest
// of the path (wildcards included) untouched.
func expandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~"+string(os.PathSeparator)) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: ~: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return home + p[1:], nil
}

// shortHost is the hostname up to the first dot, lowercased.
func shortHost(host string) string {
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(host)
}
