package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/keeper"
	"github.com/candyflipline/mnemonic/arena/obj"
	"github.com/candyflipline/mnemonic/arena/reader"
)

func TestServeCallStatus(t *testing.T) {
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
	m, _ := json.Marshal(keeper.Manifest{Tenant: "acme", Gen: 1, Seq: 1, Parts: []keeper.PartRef{{ID: "p1", Gen: 1, Key: "t/acme/p/p1.g1"}}})
	if err := s.Put(ctx, "t/acme/m/0000000000000001-0000000000000001.json", bytes.NewReader(m), int64(len(m))); err != nil {
		t.Fatal(err)
	}
	k, err := keeper.Take(ctx, s, keeper.Config{Tenant: "acme", GrantTTL: time.Minute, DeleteSlack: keeper.MinDeleteSlack})
	if err != nil {
		t.Fatal(err)
	}
	ks := keepServer{k: k, s: s, local: reader.Local(k.Parts), ttl: time.Minute}
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
