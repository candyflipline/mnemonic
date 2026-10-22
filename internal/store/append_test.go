package store

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/wire"
)

func TestAppendServerSource(t *testing.T) {
	dir := t.TempDir()
	st := &Store{DataDir: dir}
	srv := httptest.NewServer(auth.Middleware(map[string]string{auth.Hash("tok"): "chris"})(st.Handler()))
	t.Cleanup(srv.Close)
	src := ServerSource{Author: "chris", Harness: "annotation", Name: "annotation/2026-10-21"}
	k := src.key()
	head := wire.HeadHash([]byte("a\n"))

	steps := []struct {
		name       string
		requestKey string
		lines      string
		crashFirst bool
		dup        bool
		raw        string
		size       int64
	}{
		{"create", "k1", "a\n", false, false, "a\n", 2},
		{"same key again", "k1", "a\n", false, true, "a\n", 2},
		{"next key", "k2", "b\n", false, false, "a\nb\n", 4},
		{"retry after a crash before the manifest", "k3", "c\n", true, false, "a\nb\nc\n", 6},
		{"same key after the retry", "k3", "c\n", false, true, "a\nb\nc\n", 6},
	}
	for _, s := range steps {
		if s.crashFirst {
			m, err := readManifest(k.Manifest(dir))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := appendZst(k.Raw(dir), m.ZstSize, []byte(s.lines)); err != nil {
				t.Fatal(err)
			}
		}
		got, dup, err := st.Append(src, s.requestKey, []byte(s.lines))
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		want := wire.Source{SrcID: wire.SrcID(src.Name), Harness: "annotation", SourceName: src.Name, Path: src.Name,
			Size: s.size, HeadHash: head, UpdatedMs: got.UpdatedMs}
		if dup != s.dup || got != want || got.UpdatedMs <= 0 {
			t.Fatalf("%s: got %+v dup=%v, want %+v dup=%v", s.name, got, dup, want, s.dup)
		}
		if raw := rawOf(t, dir, k); raw != s.raw {
			t.Fatalf("%s: raw = %q, want %q", s.name, raw, s.raw)
		}
	}

	res := put(t, srv, rawTarget(ServerMachine, "annotation", k.SrcID, "6", head), "d\n")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("client PUT to the server machine: status %d, want 403", res.StatusCode)
	}
	if raw := rawOf(t, dir, k); raw != "a\nb\nc\n" {
		t.Fatalf("raw after refused PUT = %q", raw)
	}
}
