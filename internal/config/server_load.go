package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/wire"
)

func LoadServer(path string) (Server, error) {
	var cfg Server
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Server{}, fmt.Errorf("read server config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Server{}, fmt.Errorf("%s: %s", path, removedKey(undecoded[0]))
	}
	if cfg.DataDir == "" {
		return Server{}, fmt.Errorf("%s: data_dir must be set", path)
	}
	if cfg.QUIC.Listen == "" {
		return Server{}, fmt.Errorf("%s: [quic] listen must be set", path)
	}
	_, port, err := net.SplitHostPort(cfg.QUIC.Listen)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 {
		return Server{}, fmt.Errorf("%s: [quic] listen must be a host:port with port 1..65535", path)
	}
	if cfg.QUIC.KeyFile == "" {
		cfg.QUIC.KeyFile = filepath.Join(cfg.DataDir, "server.key")
	}
	if len(cfg.Keys) == 0 {
		return Server{}, fmt.Errorf("%s: at least one [[keys]] entry is required", path)
	}
	seen := map[string]string{}
	tenantOf := map[string]string{}
	for i, k := range cfg.Keys {
		if k.Author == "" {
			return Server{}, fmt.Errorf("%s: keys[%d]: author must be set", path, i)
		}
		if _, err := auth.ParseKey(k.Key); err != nil {
			return Server{}, fmt.Errorf("%s: keys[%d] (%s): %w", path, i, k.Author, err)
		}
		if prev, dup := seen[k.Key]; dup {
			return Server{}, fmt.Errorf("%s: keys[%d]: key already used by %s", path, i, prev)
		}
		seen[k.Key] = k.Author
		tenant := k.Tenant
		if tenant == "" {
			tenant = auth.DefaultTenant
		} else if !wire.ValidSegment(tenant) {
			return Server{}, fmt.Errorf("%s: keys[%d] (%s): tenant %q must be lowercase letters, digits, '.', '_' or '-'", path, i, k.Author, tenant)
		}
		if prev, ok := tenantOf[k.Author]; ok && prev != tenant {
			return Server{}, fmt.Errorf("%s: keys[%d]: author %s is in tenant %s and %s; an author sits in one tenant", path, i, k.Author, prev, tenant)
		}
		tenantOf[k.Author] = tenant
		if k.Scopes != nil && len(k.Scopes) == 0 {
			return Server{}, fmt.Errorf("%s: keys[%d] (%s): scopes is empty; omit it for the default", path, i, k.Author)
		}
		for _, s := range k.Scopes {
			if !slices.Contains(auth.Scopes[:], s) {
				return Server{}, fmt.Errorf("%s: keys[%d] (%s): unknown scope %q; scopes are %v", path, i, k.Author, s, auth.Scopes)
			}
		}
	}
	if cfg.Share != nil {
		if err := cfg.Share.check(); err != nil {
			return Server{}, fmt.Errorf("%s: [share] %w", path, err)
		}
	}
	return cfg, nil
}

func (s *Share) check() error {
	if s.Listen == "" {
		s.Listen = DefaultShareListen
	}
	host, port, err := net.SplitHostPort(s.Listen)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("listen must be a host:port with port 1..65535")
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen %s is not a loopback address; links reach the world only through the tunnel", s.Listen)
	}
	u, err := url.Parse(s.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("base_url %q must be an https origin such as https://share.example.com", s.BaseURL)
	}
	if u.Scheme == "http" && !IsLoopbackHost(u.Hostname()) {
		return fmt.Errorf("base_url %q is plaintext http, allowed only for a loopback host; use https", s.BaseURL)
	}
	host, port, err = net.SplitHostPort(s.QUICAddr)
	n, portErr = strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("quic_addr %q must be the host:port joining machines dial", s.QUICAddr)
	}
	return nil
}

// IsLoopbackHost is whether host (no port) names this machine: localhost or a
// loopback address. It is the one place plaintext http is allowed.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func removedKey(k toml.Key) string {
	switch top := k[0]; top {
	case "listen", "push_listen":
		return top + " is gone: the one UDP endpoint is [quic] listen"
	case "tls":
		return "[tls] is gone: QUIC pins the server key, so there is no certificate; set [quic] listen and key_file"
	case "tokens":
		return "[[tokens]] is gone: admit a client key with [[keys]] (key, author, tenant, scopes); mnemonic key prints one"
	}
	return "unknown key " + strings.Join(k, ".")
}

func (s Server) Identities() auth.Keys {
	byKey := make(map[string]auth.Grant, len(s.Keys))
	for _, k := range s.Keys {
		byKey[k.Key] = auth.Grant{Tenant: k.Tenant, Author: k.Author, Scopes: k.Scopes}
	}
	return auth.NewKeys(byKey)
}
