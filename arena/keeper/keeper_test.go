package keeper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

func servedStore(t *testing.T, dir string) obj.Store {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	s, err := obj.Open(fmt.Sprintf("file://%s?serve=http://%s", dir, addr))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go obj.Serve(ctx, s)
	for range 200 {
		if resp, err := http.Get("http://" + addr + "/stats"); err == nil {
			resp.Body.Close()
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("store did not serve")
	return nil
}

func TestKeysAndLatest(t *testing.T) {
	for _, c := range []struct {
		got  obj.Key
		want obj.Key
	}{
		{genKey("acme", 2), "t/acme/g/0000000000000002"},
		{genKey("acme", 0x1f), "t/acme/g/000000000000001f"},
		{manifestKey("acme", 1, 9), "t/acme/m/0000000000000001-0000000000000009.json"},
		{partKey("acme", "ab12", 3), "t/acme/p/ab12.g3"},
	} {
		if c.got != c.want {
			t.Errorf("key %s, want %s", c.got, c.want)
		}
	}

	ctx := context.Background()
	s := servedStore(t, t.TempDir())
	if _, ok, err := Latest(ctx, s, "acme"); ok || err != nil {
		t.Fatalf("empty tenant: ok %v err %v", ok, err)
	}
	for _, gs := range [][2]uint64{{1, 9}, {2, 1}, {1, 2}} {
		b, _ := json.Marshal(Manifest{Tenant: "acme", Gen: Gen(gs[0]), Seq: Seq(gs[1])})
		if err := s.Put(ctx, manifestKey("acme", Gen(gs[0]), Seq(gs[1])), bytes.NewReader(b), int64(len(b))); err != nil {
			t.Fatal(err)
		}
	}
	m, ok, err := Latest(ctx, s, "acme")
	if err != nil || !ok || m.Gen != 2 || m.Seq != 1 {
		t.Fatalf("Latest = %d-%d ok %v err %v, want 2-1", m.Gen, m.Seq, ok, err)
	}
}
