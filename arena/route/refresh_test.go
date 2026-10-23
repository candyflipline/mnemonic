package route

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

func TestRefreshReadsAnUnchangedManifestOnce(t *testing.T) {
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
	defer cancel()
	go obj.Serve(ctx, s)
	stats := func() (n struct{ Requests int64 }) {
		for range 200 {
			resp, err := http.Get("http://" + addr + "/stats")
			if err == nil {
				defer resp.Body.Close()
				if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("store did not serve")
		return n
	}
	stats()
	b := []byte(`{"Tenant":"acme","Gen":1,"Seq":1,"Fence":{"GrantTTL":600000000000,"DeleteSlack":60000000000,"MaxCall":30000000000}}`)
	if err := s.Put(ctx, "t/acme/m/0000000000000001-0000000000000001.json", bytes.NewReader(b), int64(len(b))); err != nil {
		t.Fatal(err)
	}
	r := NewRouter("acme", s, nil, nil)
	r.refresh(ctx)
	r.refresh(ctx)
	r.refresh(ctx)
	if l := r.listed.Load(); l == nil || l.m.Gen != 1 || l.m.Seq != 1 {
		t.Fatalf("listed %+v, want gen 1 seq 1", l)
	}
	if got := stats().Requests; got != 1 {
		t.Fatalf("three refreshes over one manifest made %d store GETs, want 1", got)
	}
}
