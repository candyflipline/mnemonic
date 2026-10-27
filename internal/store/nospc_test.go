package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/wire"
)

func TestW1NoSpaceFailsOnlyBatch(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		errno        syscall.Errno
	}{
		{"rewrite-enospc", ".b.jsonl.zst.", syscall.ENOSPC},
		{"rewrite-edquot", ".b.jsonl.zst.", syscall.EDQUOT},
		{"extend-enospc", "a.jsonl.zst", syscall.ENOSPC},
		{"backfill-edquot", ".c.json.", syscall.EDQUOT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			dir := t.TempDir()
			baseDir := t.TempDir()
			originalWrite := writeBatchFile
			defer func() { writeBatchFile = originalWrite }()
			var inject atomic.Bool
			writeBatchFile = func(f *os.File, b []byte, off int64) (int, error) {
				if strings.HasPrefix(filepath.Base(f.Name()), tc.target) && inject.CompareAndSwap(true, false) {
					n, err := originalWrite(f, b[:len(b)/2], off)
					if err != nil {
						return n, err
					}
					return n, &os.PathError{Op: "write", Path: f.Name(), Err: tc.errno}
				}
				return originalWrite(f, b, off)
			}
			b, closeFS, err := openBarrier(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer closeFS()
			entered, release := make(chan struct{}), make(chan struct{})
			var gate atomic.Bool
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			c, err := open(dir, func(paths touched) error {
				if gate.CompareAndSwap(true, false) {
					close(entered)
					<-release
				}
				return b(paths)
			}, func(error) {})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			defer unblock()
			bb, closeBase, err := openBarrier(baseDir)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBase()
			baseline, err := open(baseDir, bb, func(error) {})
			if err != nil {
				t.Fatal(err)
			}
			defer baseline.Close()
			lines := func(s string) Lines {
				l, err := ParseLines([]byte(s))
				if err != nil {
					t.Fatal(err)
				}
				return l
			}
			first, tail, replacement := lines("{\"n\":1}\n"), lines("{\"n\":2}\n"), lines("{\"n\":9}\n")
			sources := make([]ClientSource, 3)
			for i, id := range []string{"a", "b", "c"} {
				sources[i], err = NewClientSource("test", "mac", "codex", id)
				if err != nil {
					t.Fatal(err)
				}
			}
			submit := func(st *Committer, src ClientSource, op Op) *Ticket {
				ticket, err := st.Submit(ctx, src, op)
				if err != nil {
					t.Fatal(err)
				}
				return ticket
			}
			applied := func(ticket *Ticket, size int64) {
				ack, err := ticket.Wait(ctx)
				if err != nil || !ack.Valid() || ack.Verdict() != Applied || ack.State().Size != size {
					t.Fatalf("ack: valid=%v verdict=%v size=%d err=%v; want APPLIED size=%d", ack.Valid(), ack.Verdict(), ack.State().Size, err, size)
				}
			}
			seed := Replace{HeadHash: "head", SourceName: "fixture", Path: "fixture", RawEnd: 8, Lines: first}
			for _, st := range []*Committer{c, baseline} {
				for _, src := range sources {
					applied(submit(st, src, seed), 8)
				}
			}
			gate.Store(true)
			blocker := submit(c, sources[0], Backfill{Offset: 8, HeadHash: "head", RawEnd: 8})
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			extend := Extend{Offset: 8, HeadHash: "head", RawEnd: 16, Lines: tail}
			replace := Replace{HeadHash: "new", SourceName: "fixture", Path: "fixture", RawEnd: 8, Lines: replacement}
			backfill := Backfill{Offset: 8, HeadHash: "head", RawEnd: 80}
			server := ServerSource{Author: "test", Harness: "otlp", Stream: "fixture", Day: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}
			tickets := []*Ticket{submit(c, sources[0], extend), submit(c, sources[1], replace), submit(c, sources[2], backfill)}
			serverTicket, err := c.submit(ctx, server.key(), serverAppend{src: server, requestKey: "request", linesLen: int64(len(first.b)), firstHash: wire.HeadHash(first.b[:bytes.IndexByte(first.b, '\n')+1])}, encodeFrame(first), int64(len(first.b)))
			if err != nil {
				t.Fatal(err)
			}
			tickets = append(tickets, serverTicket)
			inject.Store(true)
			unblock()
			applied(blocker, 8)
			for _, ticket := range tickets {
				ack, err := ticket.Wait(ctx)
				if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, tc.errno) || ack.Valid() {
					t.Fatalf("failed ticket: valid=%v err=%v; want ENOSPC and no ack", ack.Valid(), err)
				}
			}
			select {
			case <-c.stopped:
				t.Fatal("W1 ENOSPC stopped the committer; want only the batch to fail")
			default:
			}
			if inject.Load() {
				t.Fatal("write fault was not injected")
			}
			welcome := c.Sources("test", "mac")
			wantIDs := []string{"a", "b", "c"}
			if len(welcome) != len(wantIDs) {
				t.Fatalf("Welcome length=%d; want %d", len(welcome), len(wantIDs))
			}
			for i, src := range welcome {
				if src.SrcID != wantIDs[i] || src.Size != 8 || src.RawEnd != 8 {
					t.Fatalf("Welcome[%d]=%+v; want %s size=8 raw_end=8", i, src, wantIDs[i])
				}
			}
			for _, src := range sources {
				if _, err := os.Stat(src.key.Manifest(dir)); err != nil {
					t.Fatalf("manifest %s after the failed batch: %v; want the acked one", src.key.SrcID, err)
				}
			}
			oldRaw, err := os.ReadFile(sources[0].key.Raw(dir))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(oldRaw, encodeFrame(first)) {
				t.Fatal("earlier source's raw tail survived rollback")
			}
			if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if isTemp(d.Name()) {
					t.Errorf("leftover temp: %s", path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, st := range []*Committer{c, baseline} {
				applied(submit(st, sources[0], extend), 16)
				applied(submit(st, sources[1], replace), 8)
				applied(submit(st, sources[2], backfill), 8)
				state, duplicate, err := st.Append(server, "request", first.b)
				if err != nil || duplicate || state.Size != 8 {
					t.Fatalf("server append: size=%d duplicate=%v err=%v", state.Size, duplicate, err)
				}
			}
			for _, key := range append(sources, ClientSource{key: server.key()}) {
				got, err := os.ReadFile(key.key.Raw(dir))
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(key.key.Raw(baseDir))
				if err != nil {
					t.Fatal(err)
				}
				if len(got) == 0 || !bytes.Equal(got, want) {
					t.Fatalf("raw %s differs from uninterrupted run", key.key.SrcID)
				}
			}
		})
	}
}
