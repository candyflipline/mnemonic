package intake

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

func TestSilentStreamsFreeTheirSlots(t *testing.T) {
	helloLimit = 300 * time.Millisecond
	t.Cleanup(func() { helloLimit = 60 * time.Second })

	gs, _ := Mount(nil, auth.Authors(map[string]string{auth.Hash("tok"): "chris"}))
	lis := bufconn.Listen(1 << 16)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer tok")
	var conns []*grpc.ClientConn
	for range 4 {
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conns = append(conns, conn)
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
