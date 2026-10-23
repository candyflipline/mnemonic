package index

import (
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Kind is where a source's bytes live. Only reading them differs by kind:
// the checkpoint's offset is in the kind's own bytes, and planning, parts,
// winners and tools never look at it.
type Kind uint8

const (
	// KindStore is a pushed source in the store: raw/.../<src>.jsonl.zst
	// read up to its manifest's zst_size. The offset is in compressed bytes.
	KindStore Kind = iota
	// KindPlain is a harness's own JSONL file read in place (local mode). The
	// offset is in the file's plain bytes.
	KindPlain
)

func (k Kind) open(root string, s source) (*os.File, int64, error) {
	if k == KindPlain {
		return openPlain(s)
	}
	return openRaw(root, s)
}

func (k Kind) extends(f *os.File, end int64, prev ckpt) bool {
	if k == KindPlain {
		return plainExtends(f, end, prev)
	}
	return storeExtends(f, end, prev)
}

func (k Kind) reader(f *os.File, from, end int64) (io.ReadCloser, error) {
	sec := io.NewSectionReader(f, from, end-from)
	if k == KindPlain {
		return io.NopCloser(sec), nil
	}
	zr, err := zstd.NewReader(sec, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return zr.IOReadCloser(), nil
}

// openPlain opens a KindPlain source's file in place and the end a parse may
// read to: the last newline, so a line being written is left for the next run.
func openPlain(s source) (*os.File, int64, error) { panic("lane U10") }

// plainExtends is KindPlain's byte test: the 64 KB of the file before the
// checkpoint offset hash to prev.WindowSHA; on a mismatch the source parses
// from byte 0 into gen+1.
func plainExtends(f *os.File, end int64, prev ckpt) bool { panic("lane U10") }
