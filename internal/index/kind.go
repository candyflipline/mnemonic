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

// Kind is where a source's bytes live.
type Kind uint8

const (
	// KindStore is a pushed source in the store: raw/.../<src>.jsonl.zst
	// read up to its manifest's zst_size.
	KindStore Kind = iota
	// KindPlain is a harness's own JSONL file read in place (local mode).
	KindPlain
)

func (k Kind) sweepable() bool { return k == KindStore }

func (k Kind) open(root string, s source) (*os.File, int64, error) {
	if k == KindPlain {
		return openPlain(s)
	}
	return openRaw(root, s)
}

func (k Kind) extends(f *os.File, end int64, prev ckpt) bool {
	if prev.Offset > end {
		return false
	}
	if sum, err := windowSHA(f, prev.Offset); err != nil || sum != prev.WindowSHA {
		return false
	}
	if k == KindPlain || prev.Offset == end {
		return true
	}
	magic := make([]byte, 4)
	_, err := f.ReadAt(magic, prev.Offset)
	return err == nil && bytes.Equal(magic, zstdMagic)
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
	end, err := completeLinesEnd(f, fi.Size())
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, end, nil
}

func completeLinesEnd(f *os.File, size int64) (int64, error) {
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

// redactReader is push's per-line redaction over a stream: every line read
// goes through red.Line, the exact bytes push's redactLines uploads for the
// same lines, so a local parse sees what the server path's raw holds and the
// parser's offsets and uids match it byte for byte.
type redactReader struct {
	src io.Closer
	r   *bufio.Reader
	red *redact.Redactor
	out []byte
	err error
}

func newRedactReader(r io.ReadCloser, red *redact.Redactor) *redactReader {
	return &redactReader{src: r, r: bufio.NewReaderSize(r, lightFrame), red: red}
}

func (rr *redactReader) Close() error { return rr.src.Close() }

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
