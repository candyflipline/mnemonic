// Package qnet carries the push stream and MCP over one QUIC (UDP) endpoint.
// Each side is an ed25519 key: the server accepts the client keys it lists,
// and the client pins the server's key. There is no CA, domain or bearer
// token; the TLS 1.3 handshake proves both keys.
package qnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/quic"
)

const (
	ALPNPush = "mnemonic-push/1"
	ALPNMCP  = "mnemonic-mcp/1"

	bufSize   = 16 << 20
	idle      = 2 * time.Minute
	keepalive = 20 * time.Second
)

// EncodeKey is the text form of a public key in configs and logs.
func EncodeKey(pub ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(pub) }

// LoadOrCreateKey reads the base64 ed25519 seed at path, writing a new one
// (mode 0600) when the file does not exist.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: not a base64 ed25519 seed", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(base64.RawURLEncoding.EncodeToString(priv.Seed()) + "\n"); err != nil {
		return nil, err
	}
	return priv, nil
}

func PublicKey(priv ed25519.PrivateKey) string { return EncodeKey(priv.Public().(ed25519.PublicKey)) }

func certificate(priv ed25519.PrivateKey) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

func keyOfCert(der []byte) (string, error) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	pub, ok := c.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("peer certificate is not an ed25519 key")
	}
	return EncodeKey(pub), nil
}

func onlyKey(raw [][]byte) (string, error) {
	if len(raw) != 1 {
		return "", fmt.Errorf("peer sent %d certificates, want 1", len(raw))
	}
	return keyOfCert(raw[0])
}

func config(tc *tls.Config) *quic.Config {
	return &quic.Config{
		TLSConfig:                tc,
		MaxStreamReadBufferSize:  bufSize,
		MaxStreamWriteBufferSize: bufSize,
		MaxConnReadBufferSize:    bufSize,
		MaxIdleTimeout:           idle,
		KeepAlivePeriod:          keepalive,
		RequireAddressValidation: true,
	}
}

// KeyAddr is the remote address of a qnet conn: the peer's verified key and
// its UDP address.
type KeyAddr struct {
	Key string
	UDP netip.AddrPort
}

func (a KeyAddr) Network() string { return "quic" }
func (a KeyAddr) String() string  { return "ed25519:" + a.Key + "@" + a.UDP.String() }

// KeyOf is the key in a KeyAddr's String form (http.Request.RemoteAddr).
func KeyOf(remoteAddr string) (string, bool) {
	rest, ok := strings.CutPrefix(remoteAddr, "ed25519:")
	if !ok {
		return "", false
	}
	key, _, ok := strings.Cut(rest, "@")
	return key, ok && key != ""
}

// AsBearer makes the handshake key the request's bearer token, replacing any
// Authorization header the client sent.
func AsBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := KeyOf(r.RemoteAddr)
		if !ok {
			http.Error(w, "no peer key", http.StatusUnauthorized)
			return
		}
		r.Header.Set("Authorization", "Bearer "+key)
		next.ServeHTTP(w, r)
	})
}

// Server is one UDP endpoint with a net.Listener per protocol.
type Server struct {
	ep        *quic.Endpoint
	Push, MCP net.Listener
	ctx       context.Context
	cancel    context.CancelFunc
}

// Listen binds addr. allow reports whether a client key may connect; a key
// it refuses fails the TLS handshake.
func Listen(addr string, priv ed25519.PrivateKey, allow func(key string) bool) (*Server, error) {
	cert, err := certificate(priv)
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		NextProtos:   []string{ALPNPush, ALPNMCP},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			key, err := onlyKey(raw)
			if err != nil {
				log.Printf("qnet: refused handshake: %v", err)
				return err
			}
			if !allow(key) {
				log.Printf("qnet: refused key %s", key)
				return fmt.Errorf("key %s is not allowed", key)
			}
			return nil
		},
	}
	ep, err := quic.Listen("udp", addr, config(tc))
	if err != nil {
		return nil, err
	}
	local := net.UDPAddrFromAddrPort(ep.LocalAddr())
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ep: ep, ctx: ctx, cancel: cancel, Push: newListener(local), MCP: newListener(local)}
	go s.accept()
	return s, nil
}

func (s *Server) Close() error {
	s.cancel()
	s.Push.Close()
	s.MCP.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.ep.Close(ctx)
}

func (s *Server) accept() {
	for {
		c, err := s.ep.Accept(s.ctx)
		if err != nil {
			return
		}
		go s.streams(c)
	}
}

func (s *Server) streams(c *quic.Conn) {
	for {
		st, err := c.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		cs := c.ConnectionState()
		if len(cs.PeerCertificates) != 1 {
			c.Abort(errors.New("no peer key"))
			return
		}
		pub, _ := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
		l := s.Push.(*listener)
		if cs.NegotiatedProtocol == ALPNMCP {
			l = s.MCP.(*listener)
		}
		sc := newConn(st, l.addr, KeyAddr{Key: EncodeKey(pub), UDP: c.RemoteAddr()}, nil)
		select {
		case l.ch <- sc:
		case <-l.done:
			sc.Close()
			return
		}
	}
}

// Dial opens one QUIC connection with one stream to addr, speaking alpn,
// and fails unless the server proves serverKey.
func Dial(ctx context.Context, addr string, priv ed25519.PrivateKey, serverKey, alpn string) (net.Conn, error) {
	cert, err := certificate(priv)
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			key, err := onlyKey(raw)
			if err != nil {
				return err
			}
			if key != serverKey {
				return fmt.Errorf("server key %s is not the pinned %s", key, serverKey)
			}
			return nil
		},
	}
	ep, err := quic.Listen("udp", ":0", nil)
	if err != nil {
		return nil, err
	}
	c, err := ep.Dial(ctx, "udp", addr, config(tc))
	if err != nil {
		ep.Close(context.Background())
		return nil, err
	}
	st, err := c.NewStream(ctx)
	if err != nil {
		c.Abort(nil)
		ep.Close(context.Background())
		return nil, err
	}
	local := net.UDPAddrFromAddrPort(ep.LocalAddr())
	return newConn(st, local, KeyAddr{Key: serverKey, UDP: c.RemoteAddr()}, func() {
		c.Abort(nil)
		ep.Close(context.Background())
	}), nil
}

type listener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func newListener(addr net.Addr) *listener {
	return &listener{ch: make(chan net.Conn), done: make(chan struct{}), addr: addr}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *listener) Addr() net.Addr { return l.addr }

type conn struct {
	st            *quic.Stream
	local, remote net.Addr
	closer        func()
	once          sync.Once

	mu               sync.Mutex
	rcancel, wcancel context.CancelFunc
}

func newConn(st *quic.Stream, local, remote net.Addr, closer func()) *conn {
	return &conn{st: st, local: local, remote: remote, closer: closer}
}

func timeoutErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return os.ErrDeadlineExceeded
	}
	return err
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.st.Read(b)
	return n, timeoutErr(err)
}

func (c *conn) Write(b []byte) (int, error) {
	n, err := c.st.Write(b)
	if err == nil {
		err = c.st.Flush()
	}
	return n, timeoutErr(err)
}

func (c *conn) Close() error {
	c.once.Do(func() {
		c.st.CloseRead()
		c.st.CloseWrite()
		if c.closer != nil {
			c.closer()
		}
	})
	return nil
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

func (c *conn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rcancel = setDeadline(c.rcancel, t, c.st.SetReadContext)
	return nil
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wcancel = setDeadline(c.wcancel, t, c.st.SetWriteContext)
	return nil
}

func setDeadline(prev context.CancelFunc, t time.Time, set func(context.Context)) context.CancelFunc {
	if prev != nil {
		prev()
	}
	if t.IsZero() {
		set(context.Background())
		return nil
	}
	ctx, cancel := context.WithDeadline(context.Background(), t)
	set(ctx)
	return cancel
}
