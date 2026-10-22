package store

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/wire"
)

func TestAppendServerSource(t *testing.T) {
	dir := t.TempDir()
	st := &Store{DataDir: dir}
	srv := httptest.NewServer(auth.Middleware(map[string]string{auth.Hash("tok"): "chris"})(st.Handler()))
	t.Cleanup(srv.Close)
	day1 := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	src1 := ServerSource{Author: "chris", Harness: "annotation", Day: day1}
	src2 := ServerSource{Author: "chris", Harness: "annotation", Day: day2}
	byName := map[string]ServerSource{"annotation/2026-10-21": src1, "annotation/2026-10-22": src2}

	steps := []struct {
		name       string
		src        ServerSource
		requestKey string
		lines      string
		crashFirst bool
		dup        bool
		srcName    string
		raw        string
		size       int64
	}{
		{"create", src1, "k1", "a\n", false, false, "annotation/2026-10-21", "a\n", 2},
		{"same key again", src1, "k1", "a\n", false, true, "annotation/2026-10-21", "a\n", 2},
		{"next key", src1, "k2", "b\n", false, false, "annotation/2026-10-21", "a\nb\n", 4},
		{"retry after a crash before the manifest", src1, "k3", "c\n", true, false, "annotation/2026-10-21", "a\nb\nc\n", 6},
		{"same key after the retry", src1, "k3", "c\n", false, true, "annotation/2026-10-21", "a\nb\nc\n", 6},
		{"the day before's key on the next day", src2, "k3", "c\n", false, true, "annotation/2026-10-21", "a\nb\nc\n", 6},
		{"the next day is a new source", src2, "k4", "d\n", false, false, "annotation/2026-10-22", "d\n", 2},
	}
	for _, s := range steps {
		k := s.src.key()
		if s.crashFirst {
			m, err := readManifest(k.Manifest(dir))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := appendZst(k.Raw(dir), m.ZstSize, []byte(s.lines)); err != nil {
				t.Fatal(err)
			}
		}
		got, dup, err := st.Append(s.src, s.requestKey, []byte(s.lines))
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		head := wire.HeadHash([]byte(s.raw[:2]))
		want := wire.Source{SrcID: wire.SrcID(s.srcName), Harness: "annotation", SourceName: s.srcName, Path: s.srcName,
			Size: s.size, HeadHash: head, UpdatedMs: got.UpdatedMs}
		if dup != s.dup || got != want || got.UpdatedMs <= 0 {
			t.Fatalf("%s: got %+v dup=%v, want %+v dup=%v", s.name, got, dup, want, s.dup)
		}
		if raw := rawOf(t, dir, byName[s.srcName].key()); raw != s.raw {
			t.Fatalf("%s: raw = %q, want %q", s.name, raw, s.raw)
		}
	}

	refused := []ServerSource{
		{Author: "chris", Harness: "claude-code", Day: day1},
		{Author: "chris", Harness: "annotation", Day: day1.Add(time.Hour)},
		{Author: "chris", Harness: "annotation", Day: day1.In(time.FixedZone("x", 3600))},
	}
	for _, src := range refused {
		if _, _, err := st.Append(src, "k9", []byte("z\n")); err == nil {
			t.Fatalf("Append(%+v) succeeded, want an error", src)
		}
	}

	k1 := src1.key()
	puts := []struct{ machine, harness string }{
		{ServerMachine, "annotation"},
		{testMachine, "annotation"},
		{testMachine, "otlp"},
		{testMachine, "control"},
	}
	for _, p := range puts {
		res := put(t, srv, rawTarget(p.machine, p.harness, k1.SrcID, "6", wire.HeadHash([]byte("a\n"))), "x\n")
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("client PUT to %s/%s: status %d, want 403", p.machine, p.harness, res.StatusCode)
		}
	}
	if raw := rawOf(t, dir, k1); raw != "a\nb\nc\n" {
		t.Fatalf("raw after refused PUTs = %q", raw)
	}
}
