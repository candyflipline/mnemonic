package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"

	"github.com/candyflipline/mnemonic/internal/wire"
)

func LoadClient(path string) (Client, error) {
	path, err := ExpandHome(path)
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
		tokenFile, err := ExpandHome(cfg.TokenFile)
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
		cfg.Machine = ShortHost(host)
	}
	if !wire.ValidSegment(cfg.Machine) {
		return Client{}, fmt.Errorf("config: machine %q must match [a-z0-9._-]+", cfg.Machine)
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = append([]SourceGlob(nil), DefaultSources...)
	}
	for i, src := range cfg.Sources {
		glob, err := ExpandHome(src.Glob)
		if err != nil {
			return Client{}, err
		}
		if !doublestar.ValidatePathPattern(glob) {
			return Client{}, fmt.Errorf("config: sources[%d]: bad glob %q", i, src.Glob)
		}
		cfg.Sources[i].Glob = glob
	}
	return cfg, nil
}

func ExpandHome(p string) (string, error) {
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

func ShortHost(host string) string {
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(host)
}
