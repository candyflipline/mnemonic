package push

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPutReusesConnection(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"size":10,"head_hash":"abcdefabcdefabcd","raw_end":10,"updated_ms":1,"source_name":"claude","path":"/x"}` + "\n"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	cfg := testConfig(srv.URL, t.TempDir())
	f := fileRef{harness: "claude-code", sourceName: "claude", path: "/x"}
	for i := 0; i < 20; i++ {
		if _, err := putSource(context.Background(), cfg, srv.Client(), f, "abcdefabcdefabcd", 0, "h", []byte("a\n"), 2); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("20 PUTs opened %d connections, want 1", n)
	}
}
