package cachetest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"

	_ "github.com/duckdb/duckdb-go/v2"
)

const (
	Author     = "test"
	Machine    = "m1"
	IngestedAt = "2026-01-01 00:00:00.000"
)

type golden struct {
	Harness    string `json:"harness"`
	SourceFile string `json:"source_file"`
	Records    []struct {
		EventRows []map[string]json.RawMessage `json:"event_rows"`
	} `json:"records"`
}

// Lake writes one events parquet plus its manifest per golden into dataDir,
// exactly as lake.Run would: author test, machine m1, harness from the
// golden, src_id from the golden's source_file, and the fixed ingested_at.
// The <event_version> placeholder becomes 1.
func Lake(dataDir string, goldens ...string) error {
	dir, err := goldensDir()
	if err != nil {
		return err
	}
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		return err
	}
	defer db.Close()
	tmp, err := os.MkdirTemp("", "cachetest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	for _, name := range goldens {
		raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			return err
		}
		var g golden
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		k := layout.Key{Author: Author, Machine: Machine, Harness: g.Harness}
		k.SrcID = wire.SrcID(g.SourceFile)
		if err := writeSource(db, tmp, dataDir, k, g, name); err != nil {
			return err
		}
	}
	return db.Close()
}

// Source writes one golden at an explicit layout key, so tests can place the
// same source under several authors or machines. An empty k.SrcID is derived
// from the golden's source_file.
func Source(dataDir string, k layout.Key, goldenName string) error {
	dir, err := goldensDir()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, goldenName+".json"))
	if err != nil {
		return err
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		return err
	}
	if k.Author == "" {
		k.Author = Author
	}
	if k.Machine == "" {
		k.Machine = Machine
	}
	if k.Harness == "" {
		k.Harness = g.Harness
	}
	if k.SrcID == "" {
		k.SrcID = wire.SrcID(g.SourceFile)
	}
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		return err
	}
	defer db.Close()
	tmp, err := os.MkdirTemp("", "cachetest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	return writeSource(db, tmp, dataDir, k, g, goldenName)
}

func writeSource(db *sql.DB, tmp, dataDir string, k layout.Key, g golden, name string) error {
	srcID := k.SrcID

	var rows []map[string]json.RawMessage
	sourceName := name
	for _, rec := range g.Records {
		for _, row := range rec.EventRows {
			if string(row["event_version"]) == `"<event_version>"` {
				row["event_version"] = json.RawMessage("1")
			}
			var src string
			if json.Unmarshal(row["source_name"], &src) == nil && src != "" {
				sourceName = src
			}
			row["author"] = json.RawMessage(sqlJSONString(k.Author))
			row["machine_id"] = json.RawMessage(sqlJSONString(k.Machine))
			row["ingested_at"] = json.RawMessage(`"` + IngestedAt + `"`)
			rows = append(rows, row)
		}
	}

	nd := filepath.Join(tmp, srcID+".ndjson")
	var lines strings.Builder
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		lines.Write(b)
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(nd, []byte(lines.String()), 0o644); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(k.Events(dataDir)), 0o755); err != nil {
		return err
	}
	if _, err := db.Exec("COPY (SELECT " + strings.Join(readCols(), ", ") + " FROM read_json(" +
		sqlStr(nd) + ", format='newline_delimited', columns=" + jsonColumns() + ")) TO " +
		sqlStr(k.Events(dataDir)) + " (FORMAT parquet, COMPRESSION zstd)"); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(k.Manifest(dataDir)), 0o755); err != nil {
		return err
	}
	src := wire.Source{
		SrcID: srcID, Harness: k.Harness, SourceName: sourceName, Path: g.SourceFile,
		Size: int64(lines.Len()), HeadHash: strings.Repeat("0", 64), UpdatedMs: 1767225600000,
	}
	b, err := json.Marshal(struct {
		wire.Source
		ZSTSize int64 `json:"zst_size"`
	}{src, 1})
	if err != nil {
		return err
	}
	return os.WriteFile(k.Manifest(dataDir), b, 0o644)
}

func sqlJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func readCols() []string {
	cols := make([]string, 0, len(schema.EventColumns)+3)
	for _, c := range schema.EventColumns {
		cols = append(cols, "cast("+c.Name+" AS "+c.DuckDB+") AS "+c.Name)
	}
	return append(cols, "author", "machine_id", "cast(ingested_at AS TIMESTAMP) AS ingested_at")
}

func jsonColumns() string {
	specs := make([]string, 0, len(schema.EventColumns)+3)
	for _, c := range schema.EventColumns {
		specs = append(specs, "'"+c.Name+"': '"+c.DuckDB+"'")
	}
	return "{" + strings.Join(append(specs,
		"'author': 'VARCHAR'", "'machine_id': 'VARCHAR'", "'ingested_at': 'TIMESTAMP'"), ", ") + "}"
}

func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func goldensDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cachetest: cannot locate testdata/goldens")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "goldens"), nil
}
