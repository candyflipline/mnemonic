package config

import (
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/wallet"
	"github.com/candyflipline/mnemonic/internal/wire"
)

var defaultScopes = []auth.Scope{auth.Read, auth.Push}

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
		if strings.HasPrefix(k.Author, wallet.AuthorPrefix) {
			return Server{}, fmt.Errorf("%s: keys[%d]: author %s: the prefix %s is for wallet readers", path, i, k.Author, wallet.AuthorPrefix)
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
		cfg.Keys[i].Tenant = tenant
		switch {
		case k.Scopes == nil:
			cfg.Keys[i].Scopes = defaultScopes
		case len(k.Scopes) == 0:
			return Server{}, fmt.Errorf("%s: keys[%d] (%s): scopes is empty; omit it for the default", path, i, k.Author)
		}
		for _, s := range k.Scopes {
			if !slices.Contains(auth.Scopes[:], s) {
				return Server{}, fmt.Errorf("%s: keys[%d] (%s): unknown scope %q; scopes are %v", path, i, k.Author, s, auth.Scopes)
			}
		}
	}
	return cfg, nil
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
