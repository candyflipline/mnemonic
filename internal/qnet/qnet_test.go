package qnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestQnet(t *testing.T) {
	serverKey, alice, stranger, other := newKey(t), newKey(t), newKey(t), newKey(t)
	aliceID, ok := auth.NewKeys(map[string]auth.Grant{PublicKey(alice): {Tenant: auth.DefaultTenant, Author: "alice", Scopes: []auth.Scope{auth.Read, auth.Push}}}).Identify(PublicKey(alice))
	if !ok {
		t.Fatal("alice not in table")
	}

	srv, err := Listen("127.0.0.1:0", serverKey, func(key ed25519.PublicKey, _ []byte) (auth.Identity, bool) {
		return aliceID, auth.EncodeKey(key) == PublicKey(alice)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	addr := srv.Addr().String()
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dials := []struct {
		name      string
		key       ed25519.PrivateKey
		pin       string
		wantErr   []string
		wantAlice bool
	}{
		{"allowed key", alice, PublicKey(serverKey), nil, true},
		{"unlisted key", stranger, PublicKey(serverKey), []string{"server refused our key " + PublicKey(stranger)}, false},
		{"wrong pin", alice, PublicKey(other), []string{"server proved key " + PublicKey(serverKey), "pinned key is " + PublicKey(other)}, false},
	}
	for _, d := range dials {
		c, err := Dial(dctx, addr, Credential{Key: d.key}, d.pin, ALPNPush)
		if len(d.wantErr) == 0 {
			if err != nil {
				t.Fatalf("%s: %v", d.name, err)
			}
			c.Write([]byte("x"))
			sc, err := srv.Push.Accept()
			if err != nil {
				t.Fatal(err)
			}
			id, ok := IdentityOf(sc)
			if !ok || id.Author() != "alice" {
				t.Errorf("%s: server conn identity = %q, %v; want alice", d.name, id.Author(), ok)
			}
			if _, ok := IdentityOf(c); ok {
				t.Errorf("%s: dialed conn reports an identity", d.name)
			}
			sc.Close()
			c.Close()
			continue
		}
		if err == nil {
			c.Close()
			t.Fatalf("%s: dial succeeded", d.name)
		}
		for _, w := range d.wantErr {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q lacks %q", d.name, err, w)
			}
		}
	}

	c, err := Dial(dctx, addr, Credential{Key: alice}, PublicKey(serverKey), ALPNHTTP)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("x"))
	if sc, err := srv.HTTP.Accept(); err != nil {
		t.Fatal(err)
	} else {
		sc.Close()
	}
	c.Close()

	gs := grpc.NewServer(grpc.Creds(Credentials()), grpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			id, ok := FromPeer(ctx)
			if !ok || id.Author() != "alice" {
				t.Errorf("grpc FromPeer = %q, %v; want alice", id.Author(), ok)
			}
			return next(ctx, req)
		}))
	healthpb.RegisterHealthServer(gs, health.NewServer())
	go gs.Serve(srv.Push)
	defer gs.Stop()
	cc, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, a string) (net.Conn, error) {
			return Dial(ctx, a, Credential{Key: alice}, PublicKey(serverKey), ALPNPush)
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	resp, err := healthpb.NewHealthClient(cc).Check(dctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("grpc health check = %v, %v", resp, err)
	}

	seen := make(chan string, 1)
	hs := &http.Server{
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if id, ok := IdentityOf(c); ok {
				return auth.NewContext(ctx, id)
			}
			return ctx
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen <- auth.Author(r.Context())
			w.Write([]byte("ok"))
		}),
	}
	go hs.Serve(srv.HTTP)
	defer hs.Close()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, a string) (net.Conn, error) {
		return Dial(ctx, a, Credential{Key: alice}, PublicKey(serverKey), ALPNHTTP)
	}}}
	hresp, err := hc.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	if got := <-seen; got != "alice" {
		t.Errorf("http request author = %q, want alice", got)
	}
}
