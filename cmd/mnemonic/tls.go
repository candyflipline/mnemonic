//go:build server

package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"path/filepath"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/candyflipline/mnemonic/internal/config"
)

// listen binds the HTTP and push listeners. With [tls] hosts set, both
// serve certificates from one autocert.Manager; tls-alpn-01 answers on the
// HTTP listener, port 443. Certificates are cached per ACME directory
// (autocert for Let's Encrypt prod, autocert-<directory host> otherwise), so
// a staging certificate is never found by a prod directory.
func listen(cfg config.Server) (httpL, pushL net.Listener, err error) {
	httpL, err = net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	pushL, err = net.Listen("tcp", cfg.PushListen)
	if err != nil {
		httpL.Close()
		return nil, nil, fmt.Errorf("listen on %s: %w", cfg.PushListen, err)
	}
	if len(cfg.TLS.Hosts) == 0 {
		return httpL, pushL, nil
	}
	cache := "autocert"
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.TLS.Hosts...),
	}
	if cfg.TLS.ACMEDirectory != "" {
		m.Client = &acme.Client{DirectoryURL: cfg.TLS.ACMEDirectory}
		u, _ := url.Parse(cfg.TLS.ACMEDirectory)
		cache += "-" + u.Host
	}
	m.Cache = autocert.DirCache(filepath.Join(cfg.DataDir, cache))
	tc := m.TLSConfig()
	tc.MinVersion = tls.VersionTLS12
	return tls.NewListener(httpL, tc), tls.NewListener(pushL, tc), nil
}
