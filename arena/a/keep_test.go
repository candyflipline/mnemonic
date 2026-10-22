package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/candyflipline/mnemonic/arena/keeper"
	"github.com/candyflipline/mnemonic/arena/obj"
	"github.com/candyflipline/mnemonic/arena/route"
)

func servedFileStore(t *testing.T) (obj.Store, context.Context) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	s, err := obj.Open(fmt.Sprintf("file://%s?serve=http://%s", t.TempDir(), addr))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go obj.Serve(ctx, s)
	if err := awaitServe(ctx, fmt.Sprintf("file://x?serve=http://%s", addr), make(chan error)); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func TestServeCallStatus(t *testing.T) {
	s, ctx := servedFileStore(t)
	m, _ := json.Marshal(keeper.Manifest{Tenant: "acme", Gen: 1, Seq: 1, Parts: []keeper.PartRef{{ID: "p1", Gen: 1, Key: "t/acme/p/p1.g1"}}})
	if err := s.Put(ctx, "t/acme/m/0000000000000001-0000000000000001.json", bytes.NewReader(m), int64(len(m))); err != nil {
		t.Fatal(err)
	}
	k, err := keeper.Take(ctx, s, keeper.Config{Tenant: "acme", GrantTTL: time.Minute, DeleteSlack: keeper.MinDeleteSlack})
	if err != nil {
		t.Fatal(err)
	}
	ks := keepServer{k: k, s: s, ttl: time.Minute}
	srv := httptest.NewServer(ks.mux())
	t.Cleanup(srv.Close)

	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"partial call whose cold part has no reader", `{"tool":"list_sessions","args":{"start_datetime":"2026-01-01T00:00:00Z","end_datetime":"2026-02-01T00:00:00Z"},"rung":"partial"}`, http.StatusBadGateway},
		{"tool refuses its own arguments", `{"tool":"open","args":{"id":"nope"},"rung":"whole"}`, http.StatusOK},
	} {
		res, err := http.Post(srv.URL+"/v1/call", "application/json", bytes.NewReader([]byte(c.body)))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
}

func TestCallReadsThePartsOfItsPinnedHead(t *testing.T) {
	session, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	frame := enc.EncodeAll(bytes.Repeat(session, 10), nil)
	root := t.TempDir()
	dir := filepath.Join(root, "raw", "author=a", "machine=m", "harness=claude-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSource := func(zst []byte, size int, updatedMs int64) {
		m, _ := json.Marshal(map[string]any{"src_id": "s1", "harness": "claude-code", "source_name": "claude",
			"path": "/p/s1.jsonl", "size": size, "updated_ms": updatedMs, "zst_size": len(zst)})
		if err := os.WriteFile(filepath.Join(dir, "s1.jsonl.zst"), zst, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "s1.json"), m, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSource(frame, len(session)*10, 1)

	s, ctx := servedFileStore(t)
	k, err := keeper.Take(ctx, s, keeper.Config{Tenant: "acme", Root: root, Local: t.TempDir(), Quiet: time.Hour, GrantTTL: time.Minute, DeleteSlack: keeper.MinDeleteSlack})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	ks := keepServer{k: k, s: s, ttl: time.Minute}
	args := json.RawMessage(`{"start_datetime":"2000-01-01T00:00:00Z","end_datetime":"2100-01-01T00:00:00Z"}`)

	err = k.Read(func(h keeper.Head) error {
		v, err := ks.view(h)
		if err != nil {
			return err
		}
		want, _, err := ks.do(ctx, h, v, route.Whole, "list_sessions", args)
		if err != nil {
			return err
		}
		if len(h.Hot) != 1 {
			t.Fatalf("pinned head has %d hot parts, want 1", len(h.Hot))
		}
		writeSource(append(frame, frame...), len(session)*20, 2)
		if err := k.Cycle(ctx); err != nil {
			return err
		}
		if err := k.Read(func(h2 keeper.Head) error {
			if h2.Hot[0].ID == h.Hot[0].ID {
				t.Fatalf("the commit kept hot part %s; the test needs it replaced", h.Hot[0].ID)
			}
			return nil
		}); err != nil {
			return err
		}
		got, _, err := ks.do(ctx, h, v, route.Whole, "list_sessions", args)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("a call pinned to a head answered differently after a commit replaced its hot part:\n%s\nwant\n%s", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
