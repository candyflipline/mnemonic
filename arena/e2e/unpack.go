package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

var seedDirs = map[string]string{"claude-code": "claude", "codex": "codex"}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._=-]`)

// unpack decompresses the claude-code and codex sources of a raw tree into the
// SRC layout seed reads, one <machine>-<src_id>.jsonl per source. The bytes are
// the redacted lines the server stored.
func unpack(args []string) error {
	fs := flag.NewFlagSet("unpack", flag.ExitOnError)
	raw := fs.String("raw", "", "dir holding author=*/machine=*/harness=*/<src_id>.jsonl.zst")
	src := fs.String("src", "", "dir to write claude/ and codex/ into")
	jobs := fs.Int("j", 2, "sources decompressed at once")
	fs.Parse(args)
	if *raw == "" || *src == "" {
		return fmt.Errorf("-raw and -src are required")
	}
	files, err := filepath.Glob(filepath.Join(*raw, "author=*", "machine=*", "harness=*", "*.jsonl.zst"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, d := range seedDirs {
		if err := os.MkdirAll(filepath.Join(*src, d), 0o755); err != nil {
			return err
		}
	}
	var next, written, skipped atomic.Int64
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	for range max(*jobs, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				f := files[i]
				harness := strings.TrimPrefix(filepath.Base(filepath.Dir(f)), "harness=")
				machine := strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(f))), "machine=")
				dir, ok := seedDirs[harness]
				if !ok {
					skipped.Add(1)
					continue
				}
				name := unsafeName.ReplaceAllString(machine+"-"+strings.TrimSuffix(filepath.Base(f), ".jsonl.zst"), "_") + ".jsonl"
				if err := unpackOne(f, filepath.Join(*src, dir, name)); err != nil {
					mu.Lock()
					if first == nil {
						first = fmt.Errorf("%s: %w", filepath.Base(f), err)
					}
					mu.Unlock()
					return
				}
				written.Add(1)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("unpacked=%d skipped_other_harness=%d\n", written.Load(), skipped.Load())
	return first
}

func unpackOne(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := zstd.NewReader(in, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
