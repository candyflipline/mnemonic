package index

import (
	"bufio"
	"bytes"
	"io"
	"os"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/push"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/wire"
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
// read to: the offset just past the last newline, so a line being written is
// left for the next run. A file without a complete line reads to 0.
func openPlain(s source) (*os.File, int64, error) {
	f, err := os.Open(s.src.Path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	end, err := lastNewline(f, fi.Size())
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, end, nil
}

// lastNewline is the offset just past the last '\n' in the first size bytes
// of f, 0 when there is none. It reads backward from the end, so a growing
// file costs one short read.
func lastNewline(f *os.File, size int64) (int64, error) {
	buf := make([]byte, window)
	for at := size; at > 0; {
		step := min(int64(len(buf)), at)
		at -= step
		chunk := buf[:step]
		if _, err := f.ReadAt(chunk, at); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return at + int64(i) + 1, nil
		}
	}
	return 0, nil
}

// plainExtends is KindPlain's byte test: the 64 KB of the file before the
// checkpoint offset hash to prev.WindowSHA; on a mismatch the source parses
// from byte 0 into gen+1. The offset itself is at a newline boundary by
// construction, and extends already refused a checkpoint past the input.
func plainExtends(f *os.File, end int64, prev ckpt) bool {
	sum, err := windowSHA(f, prev.ZstSize)
	return err == nil && sum == prev.WindowSHA
}

// redactReader is push's per-line redaction over a stream: every line read
// goes through red.Line, the exact bytes push's redactLines uploads for the
// same lines, so a local parse sees what the server path's raw holds and the
// parser's offsets and uids match it byte for byte.
type redactReader struct {
	r   *bufio.Reader
	red *redact.Redactor
	out []byte
	err error
}

func newRedactReader(r io.Reader, red *redact.Redactor) *redactReader {
	return &redactReader{r: bufio.NewReaderSize(r, lightFrame), red: red}
}

// Close closes nothing: the wrapped reader's Close runs in ingestSource.
func (rr *redactReader) Close() error { return nil }

func (rr *redactReader) Read(p []byte) (int, error) {
	for len(rr.out) == 0 {
		if rr.err != nil {
			return 0, rr.err
		}
		line, err := rr.r.ReadBytes('\n')
		rr.err = err
		if len(line) == 0 {
			continue
		}
		if line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
			rr.out = rr.red.Line(line)
			rr.out = append(rr.out, '\n')
		} else {
			rr.out = rr.red.Line(line)
		}
	}
	n := copy(p, rr.out)
	rr.out = rr.out[n:]
	return n, nil
}

// listPlainSources is every harness file local mode indexes in place, as
// sources of KindPlain: the key's author and machine are the local identity,
// its src_id the file's stable id as push names it, and src the manifest push
// would have sent (path, harness, source_name, size, updated_ms = mtime).
// The globs are the client config's, since a glob string alone carries no
// harness or source name.
func listPlainSources(globs []config.SourceGlob, author, machine string) ([]source, error) {
	files := push.ScanFiles(globs)
	out := make([]source, 0, len(files))
	for _, f := range files {
		srcID := wire.SrcID(f.Path)
		out = append(out, source{
			kind:  KindPlain,
			mpath: f.Path,
			key:   layout.Key{Author: author, Machine: machine, Harness: f.Harness, SrcID: srcID},
			src: manifest{Source: wire.Source{
				SrcID: srcID, Harness: f.Harness, SourceName: f.SourceName,
				Path: f.Path, Size: f.Size, UpdatedMs: f.MtimeMs,
			}},
		})
	}
	return out, nil
}
