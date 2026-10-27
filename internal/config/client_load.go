package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const quicScheme = "quic://"

const DefaultKeyFile = "~/.mnemonic/key"

// Addr is the host:port of Server; LoadClient has checked it.
func (c Client) Addr() string { return strings.TrimPrefix(c.Server, quicScheme) }

func LoadClient(path string) (Client, error) {
	path, err := ExpandHome(path)
	if err != nil {
		return Client{}, err
	}
	var cfg Client
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Client{}, fmt.Errorf("config: %w", err)
	}
	for _, key := range meta.Undecoded() {
		switch key.String() {
		case "token", "token_file":
			return Client{}, fmt.Errorf("config: %s: %s is gone: keys replaced tokens: run `mnemonic key` and set server_key", path, key)
		case "push_address":
			return Client{}, fmt.Errorf("config: %s: push_address is gone: set server = \"quic://host:port\"", path)
		}
		return Client{}, fmt.Errorf("config: %s: unknown key %q", path, key.String())
	}
	if cfg.Server == "" {
		return Client{}, fmt.Errorf("config: %s: server is required", path)
	}
	hostport, ok := strings.CutPrefix(cfg.Server, quicScheme)
	if !ok {
		return Client{}, fmt.Errorf("config: %s: server %q must be quic://host:port", path, cfg.Server)
	}
	if host, port, err := net.SplitHostPort(hostport); err != nil || host == "" || port == "" {
		return Client{}, fmt.Errorf("config: %s: server %q must be quic://host:port", path, cfg.Server)
	}
	if cfg.ServerKey == "" {
		return Client{}, fmt.Errorf("config: %s: server_key is required: run `mnemonic key --file <server key file>` on the server", path)
	}
	if _, err := auth.ParseKey(cfg.ServerKey); err != nil {
		return Client{}, fmt.Errorf("config: %s: server_key: %w", path, err)
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = DefaultKeyFile
	}
	if cfg.KeyFile, err = ExpandHome(cfg.KeyFile); err != nil {
		return Client{}, err
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
