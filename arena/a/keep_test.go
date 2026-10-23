package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	for range 200 {
		if resp, err := http.Get("http://" + addr + "/stats"); err == nil {
			resp.Body.Close()
			return s, ctx
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("store did not serve")
	return nil, nil
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
	args := json.RawMessage(`{"start_datetime":"2000-01-01T00:00:00Z","end_datetime":"2100-01-01T00:00:00Z"}`)

	err = k.Read(func(h keeper.Head) error {
		call := func() ([]byte, error) {
			var parts []route.GrantedPart
			for _, p := range h.Hot {
				parts = append(parts, route.GrantedPart{PartRef: p, Local: true})
			}
			body, _ := json.Marshal(route.CallRequest{Tenant: "acme", Parts: parts, Tool: "list_sessions", Args: args, Phase: route.PhaseWhole})
			srv := httptest.NewServer(callHandler(h))
			defer srv.Close()
			res, err := http.Post(srv.URL+"/v1/call", "application/json", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()
			var resp route.CallResponse
			if res.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("status %d", res.StatusCode)
			}
			err = json.NewDecoder(res.Body).Decode(&resp)
			return resp.Data, err
		}
		want, err := call()
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
		got, err := call()
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

func TestServesTheStoredHeadBeforeTheFirstCycleEnds(t *testing.T) {
	s, ctx := servedFileStore(t)
	m, _ := json.Marshal(keeper.Manifest{Tenant: "acme", Gen: 1, Seq: 1, Parts: []keeper.PartRef{{ID: "p1", Gen: 1, Key: "t/acme/p/p1.g1"}}})
	if err := s.Put(ctx, "t/acme/m/0000000000000001-0000000000000001.json", bytes.NewReader(m), int64(len(m))); err != nil {
		t.Fatal(err)
	}
	if err := s.PutExclusive(ctx, "t/acme/g/0000000000000001", bytes.NewReader(nil), 0); err != nil {
		t.Fatal(err)
	}
	k, err := keeper.Take(ctx, s, keeper.Config{Tenant: "acme", GrantTTL: time.Minute, DeleteSlack: keeper.MinDeleteSlack})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(ctx)
	entered, release := make(chan struct{}), make(chan struct{})
	first := func(context.Context) error {
		close(entered)
		<-release
		return nil
	}
	done := make(chan error, 1)
	ks := keepServer{k: k}
	go func() { done <- ks.serve(ctx, l, time.Hour, first, make(chan error, 3)) }()
	t.Cleanup(func() {
		close(release)
		stop()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v", err)
		}
	})
	<-entered

	c := http.Client{Timeout: 10 * time.Second}
	res, err := c.Get("http://" + l.Addr().String() + "/v1/head")
	if err != nil {
		t.Fatalf("GET /v1/head while the first cycle runs: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "no commit at gen") {
		t.Errorf("head = %d %q, want 503 with %q", res.StatusCode, body, "no commit at gen")
	}
}
