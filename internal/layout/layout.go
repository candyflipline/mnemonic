// Package layout names every path under the server's data dir. Each author
// and machine writes only under its own prefix, so pushes never contend.
//
//	raw/author=A/machine=M/harness=H/<src_id>.jsonl.zst   redacted raw lines, zstd frames appended
//	raw/author=A/machine=M/harness=H/<src_id>.json        wire.Source, the manifest entry
//	events/author=A/machine=M/harness=H/<src_id>.parquet       normalized events up to the base's checkpoint
//	events/author=A/machine=M/harness=H/<src_id>.tail.parquet  events appended since, when any
//	events/.parser-version                                     each parser's Version the whole lake was last swept at
//	cache/index.duckdb                                         derived; serve converges it from events/
package layout

import "path/filepath"

// ArchiveMachine is the machine prefix for rows imported from ClickHouse
// whose source files no longer exist anywhere.
const ArchiveMachine = "archive-ch"

// EventsGlob matches every lake file for DuckDB read_parquet with
// hive_partitioning, which yields the author, machine and harness columns.
const EventsGlob = "events/author=*/machine=*/harness=*/*.parquet"

type Key struct {
	Author, Machine, Harness, SrcID string
}

func (k Key) dir(root, kind string) string {
	return filepath.Join(root, kind, "author="+k.Author, "machine="+k.Machine, "harness="+k.Harness)
}

func (k Key) Raw(root string) string      { return filepath.Join(k.dir(root, "raw"), k.SrcID+".jsonl.zst") }
func (k Key) Manifest(root string) string { return filepath.Join(k.dir(root, "raw"), k.SrcID+".json") }
func (k Key) Events(root string) string {
	return filepath.Join(k.dir(root, "events"), k.SrcID+".parquet")
}
func (k Key) Tail(root string) string {
	return filepath.Join(k.dir(root, "events"), k.SrcID+".tail.parquet")
}

// ParserMarker holds, as JSON by harness, each parser's Version the whole lake
// was last fully swept at.
func ParserMarker(root string) string { return filepath.Join(root, "events", ".parser-version") }

func CacheDir(root string) string { return filepath.Join(root, "cache") }
