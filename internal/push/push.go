// Package push is the client side of the wire protocol: scan the configured
// globs, diff against the server's state from Welcome, send only whole
// redacted lines over one gRPC stream.
package push

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
)

type Redactor interface {
	AppendLine(dst, line []byte) []byte
}

type Stats struct {
	Scanned, Appended, Replaced, Unchanged, Failed int
	Bytes                                          int64
}

const skewMs = 5 * 60 * 1000

type fileRef struct {
	path       string
	harness    string
	sourceName string
	mtimeMs    int64
	size       int64
	head       *rawHead
}

func Run(ctx context.Context, cfg config.Client, red Redactor) (Stats, error) {
	u, err := dial(ctx, cfg)
	if err != nil {
		return Stats{}, err
	}
	var st Stats
	files := scan(cfg.Sources)
	st.Scanned = len(files)
	for round := 0; ; round++ {
		for _, f := range files {
			if err := streamSource(ctx, red, u, f, &st); err != nil {
				var se streamErr
				if errors.As(err, &se) {
					u.cancel()
					return st, err
				}
				st.Failed++
			}
		}
		if err := u.drain(ctx); err != nil {
			u.cancel()
			return st, err
		}
		redo := u.takeRedo()
		if len(redo) == 0 {
			break
		}
		if round == 1 {
			st.Failed += len(redo)
			break
		}
		files = refsOf(files, redo)
	}
	return st, u.close(ctx)
}

func refsOf(files []fileRef, paths []string) []fileRef {
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
	var out []fileRef
	for _, f := range files {
		if want[f.path] {
			out = append(out, f)
		}
	}
	return out
}

func (s Stats) String() string {
	return fmt.Sprintf("scanned %d, appended %d, replaced %d, unchanged %d, failed %d, sent %d bytes",
		s.Scanned, s.Appended, s.Replaced, s.Unchanged, s.Failed, s.Bytes)
}

var maxChunk = 32 << 20

func firstChunk(body []byte, max int) []byte {
	if len(body) <= max {
		return body
	}
	if i := bytes.LastIndexByte(body[:max], '\n'); i >= 0 {
		return body[:i+1]
	}
	return body[:bytes.IndexByte(body, '\n')+1]
}

func decide(entry *wire.Source, head string, redacted []byte) (body []byte, offset int64, noop bool) {
	if entry == nil || entry.HeadHash != head || int64(len(redacted)) < entry.Size {
		return redacted, 0, false
	}
	if int64(len(redacted)) == entry.Size {
		return nil, 0, true
	}
	cut, onBoundary := cutAtSize(redacted, entry.Size)
	if !onBoundary {
		return redacted, 0, false
	}
	return redacted[cut:], entry.Size, false
}

func rawEndOf(rawEnds []int64, redacted []byte, start int64, body []byte) int64 {
	lines := bytes.Count(redacted[:start], []byte{'\n'}) + bytes.Count(body, []byte{'\n'})
	return rawEnds[lines-1]
}

func scan(sources []config.SourceGlob) []fileRef {
	seen := make(map[string]bool)
	var files []fileRef
	for _, src := range sources {
		matches, err := doublestar.FilepathGlob(src.Glob)
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, m := range matches {
			abs, err := filepath.Abs(m)
			if err != nil || seen[abs] {
				continue
			}
			info, err := os.Stat(abs)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			seen[abs] = true
			files = append(files, fileRef{path: abs, harness: src.Harness, sourceName: src.SourceName, mtimeMs: info.ModTime().UnixMilli(), size: info.Size(), head: new(rawHead)})
		}
	}
	return files
}

type File struct {
	Path       string
	Harness    string
	SourceName string
	MtimeMs    int64
	Size       int64
}

// ScanFiles matches sources' globs the way a push scan does and returns the
// regular files found, deduplicated by absolute path, in scan order. Local
// mode names its plain sources from it, so their src ids and manifest fields
// are what push would have sent for the same file.
func ScanFiles(sources []config.SourceGlob) []File {
	refs := scan(sources)
	out := make([]File, 0, len(refs))
	for _, f := range refs {
		out = append(out, File{Path: f.path, Harness: f.harness, SourceName: f.sourceName, MtimeMs: f.mtimeMs, Size: f.size})
	}
	return out
}
