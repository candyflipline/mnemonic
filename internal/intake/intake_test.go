package intake

import (
	"context"
	"crypto/ed25519"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/qnet"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

func listen(t *testing.T, st *store.Committer, grants map[string]auth.Grant) (addr, serverKey string) {
	t.Helper()
	priv, err := qnet.LoadOrCreateKey(t.TempDir() + "/server.key")
	if err != nil {
		t.Fatal(err)
	}
	qs, err := qnet.Listen("127.0.0.1:0", priv, func(device ed25519.PublicKey) (auth.Identity, bool) {
		return auth.NewKeys(grants).Identify(auth.EncodeKey(device))
	})
	if err != nil {
		t.Fatal(err)
	}
	gs, _ := Mount(st)
	go gs.Serve(qs.Push)
	t.Cleanup(func() { gs.Stop(); qs.Close() })
	return qs.Addr().String(), qnet.PublicKey(priv)
}

func client(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv, qnet.PublicKey(priv)
}

func dial(t *testing.T, addr, serverKey string, priv ed25519.PrivateKey) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///quic",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return qnet.Dial(ctx, addr, priv, serverKey, qnet.ALPNPush)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestSilentStreamsFreeTheirSlots(t *testing.T) {
	helloLimit = 300 * time.Millisecond
	t.Cleanup(func() { helloLimit = 60 * time.Second })

	priv, key := client(t)
	addr, serverKey := listen(t, nil, map[string]auth.Grant{key: {Author: "chris"}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var conns []*grpc.ClientConn
	for range 4 {
		conns = append(conns, dial(t, addr, serverKey, priv))
	}
	open := func(i int) pushpb.Push_SyncClient {
		c, err := pushpb.NewPushClient(conns[i%len(conns)]).Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	var silent []pushpb.Push_SyncClient
	for i := range maxStreams {
		silent = append(silent, open(i))
	}
	for i, c := range silent {
		_, err := c.Recv()
		if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "no Hello within") {
			t.Fatalf("silent stream %d ended with %v, want DeadlineExceeded \"no Hello within\"", i, err)
		}
	}
	_, err := open(0).Recv()
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "no Hello within") {
		t.Fatalf("stream after the silent ones got %v, want it admitted and ended with DeadlineExceeded \"no Hello within\"", err)
	}
}

func TestPushIdentityComesFromTheKey(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pusherPriv, pusher := client(t)
	readerPriv, reader := client(t)
	addr, serverKey := listen(t, st, map[string]auth.Grant{
		pusher: {Author: "alice"},
		reader: {Author: "rita", Scopes: []auth.Scope{auth.Read}},
	})

	line := []byte(`{"type":"user"}` + "\n")
	srcID := wire.SrcID("/x/forged.jsonl")
	sync := func(priv ed25519.PrivateKey, md metadata.MD) (pushpb.Push_SyncClient, context.Context) {
		ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), md), 10*time.Second)
		t.Cleanup(cancel)
		c, err := pushpb.NewPushClient(dial(t, addr, serverKey, priv)).Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return c, ctx
	}

	t.Run("push scope stores under the key's author, metadata ignored", func(t *testing.T) {
		c, _ := sync(pusherPriv, metadata.Pairs("authorization", "Bearer bob", "x-author", "bob"))
		if err := c.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Hello{Hello: &pushpb.Hello{Machine: "m1"}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Recv(); err != nil {
			t.Fatalf("welcome: %v", err)
		}
		if err := c.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Append{Append: &pushpb.Append{
			Seq: 1, Harness: "claude-code", SrcId: srcID, HeadHash: wire.HeadHash(line),
			SourceName: "claude", Path: "/x/forged.jsonl", RawEnd: int64(len(line)), Body: line,
		}}}); err != nil {
			t.Fatal(err)
		}
		m, err := c.Recv()
		if err != nil || m.GetAck().GetResult() != pushpb.Ack_APPLIED {
			t.Fatalf("ack: %v, %v", m, err)
		}
		for author, want := range map[string]bool{"alice": true, "bob": false} {
			_, err := os.Stat(layout.Key{Author: author, Machine: "m1", Harness: "claude-code", SrcID: srcID}.Raw(dir))
			if (err == nil) != want {
				t.Errorf("raw under author %s exists = %v, want %v", author, err == nil, want)
			}
		}
	})

	t.Run("a key without push is PermissionDenied", func(t *testing.T) {
		c, _ := sync(readerPriv, nil)
		_, err := c.Recv()
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "key lacks scope push" {
			t.Fatalf("got %v, want PermissionDenied \"key lacks scope push\"", err)
		}
	})
}
