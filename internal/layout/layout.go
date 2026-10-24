// Package layout names every path under the server's data dir. Each author
// and machine writes only under its own prefix, so pushes never contend.
//
//	raw/author=A/machine=M/harness=H/<src_id>.jsonl.zst   redacted raw lines, zstd frames appended
//	raw/author=A/machine=M/harness=H/<src_id>.json        wire.Source, the manifest entry
//	index/                                                the engine's store (internal/index), derived from raw/
package layout

import "path/filepath"

// ArchiveMachine is the machine prefix of rows imported from ClickHouse by
// the 59796bc import-ch (cmd/mnemonic/server.go:93) whose source files no
// longer exist anywhere; the archive source kind reads the ch-* parquet it
// left under events/.
const ArchiveMachine = "archive-ch"

type Key struct {
	Author, Machine, Harness, SrcID string
}

func (k Key) dir(root, kind string) string {
	return filepath.Join(root, kind, "author="+k.Author, "machine="+k.Machine, "harness="+k.Harness)
}

func (k Key) Raw(root string) string      { return filepath.Join(k.dir(root, "raw"), k.SrcID+".jsonl.zst") }
func (k Key) Manifest(root string) string { return filepath.Join(k.dir(root, "raw"), k.SrcID+".json") }

// Events is the source's path in the lake layout. Nothing writes or reads
// events parquet any more; internal/index uses the path as the winner rule's
// source order key (lake path order).
func (k Key) Events(root string) string {
	return filepath.Join(k.dir(root, "events"), k.SrcID+".parquet")
}

// IndexDir is the engine's store: frame logs, parts and CURRENT.
func IndexDir(root string) string { return filepath.Join(root, "index") }
