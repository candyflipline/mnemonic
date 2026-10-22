package cache

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/lake"
	"github.com/candyflipline/mnemonic/internal/layout"
	_ "github.com/candyflipline/mnemonic/internal/normalize/annotation"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

func TestAnnotationSentOnTwoDaysIsOneRow(t *testing.T) {
	dataDir := t.TempDir()
	line := `{"id":"a1","target":{"session_id":"s1"},"name":"quality","value":{"number":4},"source":"human","ts":"2026-10-21T10:00:00Z"}` + "\n"
	for _, day := range []string{"annotation/2026-10-21", "annotation/2026-10-23"} {
		key := layout.Key{Author: "alice", Machine: store.ServerMachine, Harness: "annotation", SrcID: wire.SrcID(day)}
		if err := os.MkdirAll(filepath.Dir(key.Raw(dataDir)), 0o755); err != nil {
			t.Fatal(err)
		}
		raw, err := os.Create(key.Raw(dataDir))
		if err != nil {
			t.Fatal(err)
		}
		zw, _ := zstd.NewWriter(raw)
		zw.Write([]byte(line))
		zw.Close()
		info, _ := raw.Stat()
		raw.Close()
		m, _ := json.Marshal(map[string]any{"src_id": key.SrcID, "harness": key.Harness, "source_name": day,
			"path": day, "size": len(line), "head_hash": "h", "updated_ms": time.Now().UnixMilli(), "zst_size": info.Size()})
		if err := os.WriteFile(key.Manifest(dataDir), m, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lake.Run(dataDir, func() bool { return false }, lake.Sweep{}); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.Converge(context.Background(), RetryFailed); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := ix.DB().QueryRow("SELECT count(*) || ' ' || min(annotation_id) || ' ' || min(author) FROM annotations").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "1 a1 alice" {
		t.Fatalf("annotations: %s, want 1 a1 alice", got)
	}
}
