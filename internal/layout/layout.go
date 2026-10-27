package layout

import "path/filepath"

const ArchiveMachine = "archive-ch"

type Key struct {
	Author, Machine, Harness, SrcID string
}

func (k Key) dir(root, kind string) string {
	return filepath.Join(root, kind, "author="+k.Author, "machine="+k.Machine, "harness="+k.Harness)
}

func (k Key) Raw(root string) string      { return filepath.Join(k.dir(root, "raw"), k.SrcID+".jsonl.zst") }
func (k Key) Manifest(root string) string { return filepath.Join(k.dir(root, "raw"), k.SrcID+".json") }

// Events is the source's path in the lake layout; internal/index orders a
// uid's copies by it.
func (k Key) Events(root string) string {
	return filepath.Join(k.dir(root, "events"), k.SrcID+".parquet")
}

func IndexDir(root string) string { return filepath.Join(root, "index") }
