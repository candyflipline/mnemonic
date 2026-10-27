// Package qnet carries gRPC push and HTTP over one QUIC (UDP) endpoint. Each
// side is an ed25519 key: the client pins the server's key, the server asks
// Identify about the client's key and the binding bytes its certificate
// carries, and the TLS 1.3 handshake proves both keys. The Identity Identify
// returns is attached to every conn the server accepts. qnet does not read
// the binding; package wallet does.
package qnet

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/quic-go/quic-go"
)

const (
	ALPNPush = "mnemonic-push/1"
	ALPNHTTP = "mnemonic-http/1"
)

// Push streams sit idle for minutes. A ping every 15 s keeps UDP NAT
// mappings (often 30 s) open, and 45 s of silence (three lost pings) ends a
// dead connection.
const (
	keepAlive   = 15 * time.Second
	idleTimeout = 45 * time.Second
)

func quicConfig() *quic.Config {
	return &quic.Config{
		KeepAlivePeriod:       keepAlive,
		MaxIdleTimeout:        idleTimeout,
		MaxIncomingStreams:    8,
		MaxIncomingUniStreams: 1,
		Allow0RTT:             false,
	}
}

type Server struct {
	Push, HTTP net.Listener

	ln       *quic.Listener
	tr       *quic.Transport
	pc       net.PacketConn
	identify Identify
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
}

// Identify maps a client's proven device key and the binding its certificate
// carries (nil when none) to its Identity. It runs twice per connection, in
// the handshake, where a refusal is a TLS alert, and again when the conn is
// served, so it reads live state and holds no cache of its own.
type Identify func(device ed25519.PublicKey, binding []byte) (auth.Identity, bool)

// Listen binds the UDP address addr. identify maps a client to its Identity;
// a client it refuses fails its handshake.
func Listen(addr string, priv ed25519.PrivateKey, identify Identify) (*Server, error) {
	cert, err := certificate(Credential{Key: priv})
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{cert},
		ClientAuth:             tls.RequireAnyClientCert,
		NextProtos:             []string{ALPNPush, ALPNHTTP},
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			key, binding, err := peerCert(raw)
			if err != nil {
				return err
			}
			if _, ok := identify(key, binding); !ok {
				return fmt.Errorf("client key %s is not allowed", auth.EncodeKey(key))
			}
			return nil
		},
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	resetKey := quic.StatelessResetKey(sha256.Sum256(append([]byte("mnemonic qnet stateless reset\x00"), priv.Seed()...)))
	tr := &quic.Transport{
		Conn:                pc,
		StatelessResetKey:   &resetKey,
		VerifySourceAddress: func(net.Addr) bool { return true },
	}
	ln, err := tr.Listen(tc, quicConfig())
	if err != nil {
		tr.Close()
		pc.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ln: ln, tr: tr, pc: pc, identify: identify, ctx: ctx, cancel: cancel,
		Push: newListener(ln.Addr()), HTTP: newListener(ln.Addr())}
	go s.accept()
	return s, nil
}

func (s *Server) Addr() net.Addr { return s.ln.Addr() }

func (s *Server) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.Push.Close()
		s.HTTP.Close()
		s.ln.Close()
		s.tr.Close()
		s.pc.Close()
	})
	return nil
}

func (s *Server) accept() {
	for {
		qc, err := s.ln.Accept(s.ctx)
		if err != nil {
			return
		}
		go s.serve(qc)
	}
}

func (s *Server) serve(qc *quic.Conn) {
	cs := qc.ConnectionState()
	l := map[string]*listener{ALPNPush: s.Push.(*listener), ALPNHTTP: s.HTTP.(*listener)}[cs.TLS.NegotiatedProtocol]
	var key ed25519.PublicKey
	var binding []byte
	var id auth.Identity
	ok := l != nil
	if ok {
		var err error
		key, binding, err = peerOf(cs.TLS)
		ok = err == nil
	}
	if ok {
		id, ok = s.identify(key, binding)
	}
	if !ok {
		qc.CloseWithError(1, "refused")
		return
	}
	confirm, err := qc.OpenUniStream()
	if err != nil {
		return
	}
	confirm.Write([]byte{1})
	confirm.Close()
	for {
		st, err := qc.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		c := &conn{st: st, qc: qc, id: id, served: true}
		select {
		case l.ch <- c:
		case <-l.done:
			c.Close()
			return
		}
	}
}

func peerOf(cs tls.ConnectionState) (ed25519.PublicKey, []byte, error) {
	raw := make([][]byte, len(cs.PeerCertificates))
	for i, c := range cs.PeerCertificates {
		raw[i] = c.Raw
	}
	return peerCert(raw)
}

// Dial opens a QUIC connection and one stream to addr speaking alpn as cred.
// It fails unless the server proves serverKey and accepts cred.
func Dial(ctx context.Context, addr string, cred Credential, serverKey string, alpn string) (net.Conn, error) {
	priv := cred.Key
	cert, err := certificate(cred)
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pub, _, err := peerCert(raw)
			if err != nil {
				return err
			}
			if key := auth.EncodeKey(pub); key != serverKey {
				return fmt.Errorf("server proved key %s, pinned key is %s", key, serverKey)
			}
			return nil
		},
	}
	qc, err := quic.DialAddr(ctx, addr, tc, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("qnet dial %s: %w", addr, err)
	}
	if err := confirmed(ctx, qc); err != nil {
		qc.CloseWithError(0, "")
		return nil, fmt.Errorf("qnet dial %s: server refused our key %s: %w", addr, PublicKey(priv), err)
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		qc.CloseWithError(0, "")
		return nil, fmt.Errorf("qnet dial %s: %w", addr, err)
	}
	return &conn{st: st, qc: qc}, nil
}

// confirmed waits for the byte the server sends once it has accepted our
// key: the client side of the handshake is complete before the server's
// verdict reaches it.
func confirmed(ctx context.Context, qc *quic.Conn) error {
	us, err := qc.AcceptUniStream(ctx)
	if err != nil {
		return err
	}
	var b [1]byte
	_, err = io.ReadFull(us, b[:])
	return err
}

type listener struct {
	ch   chan *conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func newListener(addr net.Addr) *listener {
	return &listener{ch: make(chan *conn), done: make(chan struct{}), addr: addr}
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
