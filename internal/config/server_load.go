package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/wire"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	if cfg.PushListen == "" {
		return Server{}, fmt.Errorf("%s: push_listen must be set", path)
	}
	_, port, err := net.SplitHostPort(cfg.PushListen)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 {
		return Server{}, fmt.Errorf("%s: push_listen must be a host:port with port 1..65535", path)
	}
	if cfg.PushListen == cfg.Listen {
		return Server{}, fmt.Errorf("%s: push_listen must differ from listen", path)
	}
	if err := cfg.TLS.check(cfg.Listen); err != nil {
		return Server{}, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Tokens) == 0 {
		return Server{}, fmt.Errorf("%s: at least one [[tokens]] entry is required", path)
	}
	seen := map[string]string{}
	tenantOf := map[string]string{}
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
		tenant := t.Tenant
		if tenant == "" {
			tenant = auth.DefaultTenant
		} else if !wire.ValidSegment(tenant) {
			return Server{}, fmt.Errorf("%s: tokens[%d] (%s): tenant %q must be lowercase letters, digits, '.', '_' or '-'", path, i, t.Author, tenant)
		}
		if prev, ok := tenantOf[t.Author]; ok && prev != tenant {
			return Server{}, fmt.Errorf("%s: tokens[%d]: author %s is in tenant %s and %s; an author sits in one tenant", path, i, t.Author, prev, tenant)
		}
		tenantOf[t.Author] = tenant
		if t.Scopes != nil && len(t.Scopes) == 0 {
			return Server{}, fmt.Errorf("%s: tokens[%d] (%s): scopes is empty; omit it for the default", path, i, t.Author)
		}
		for _, s := range t.Scopes {
			if !slices.Contains(auth.Scopes[:], s) {
				return Server{}, fmt.Errorf("%s: tokens[%d] (%s): unknown scope %q; scopes are %v", path, i, t.Author, s, auth.Scopes)
			}
		}
	}
	return cfg, nil
}

func (t TLS) check(listen string) error {
	if len(t.Hosts) == 0 {
		if t.ACMEDirectory != "" {
			return fmt.Errorf("tls.acme_directory is set but tls.hosts is empty")
		}
		return nil
	}
	if _, port, _ := net.SplitHostPort(listen); port != "443" {
		return fmt.Errorf("tls.hosts needs listen on port 443 (tls-alpn-01 validates there), not %q", listen)
	}
	for i, h := range t.Hosts {
		if h == "" || strings.ContainsAny(h, ":/ ") {
			return fmt.Errorf("tls.hosts[%d] = %q must be a bare DNS name", i, h)
		}
	}
	if t.ACMEDirectory != "" {
		if u, err := url.Parse(t.ACMEDirectory); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("tls.acme_directory %q must be an https URL", t.ACMEDirectory)
		}
	}
	return nil
}

// Identities is the token table LoadServer validated.
func (s Server) Identities() auth.Tokens {
	byHash := make(map[string]auth.Token, len(s.Tokens))
	for _, t := range s.Tokens {
		byHash[t.SHA256] = auth.Token{Tenant: t.Tenant, Author: t.Author, Scopes: t.Scopes}
	}
	return auth.NewTokens(byHash)
}
