package main

// Query views are process-local file-backed mappings. Their pointers are never
// persisted or exposed across processes. All pointers stored in them point to
// other mapped memory, or to segments explicitly retained by Store.segments.
import (
	"fmt"
	"os"
	"sort"
	"syscall"
	"unsafe"
)

type mappedArena struct {
	blocks [][]byte
	used   int
	dir    string
}

func (a *mappedArena) alloc(n, align int) []byte {
	if n == 0 {
		return nil
	}
	off := (a.used + align - 1) &^ (align - 1)
	if len(a.blocks) == 0 || off+n > len(a.blocks[len(a.blocks)-1]) {
		size := max(1<<20, (n+4095)&^4095)
		f, err := os.CreateTemp(a.dir, ".view-")
		if err != nil {
			panic(err)
		}
		name := f.Name()
		if err = f.Truncate(int64(size)); err != nil {
			f.Close()
			os.Remove(name)
			panic(err)
		}
		b, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		f.Close()
		os.Remove(name)
		if err != nil {
			panic(err)
		}
		a.blocks = append(a.blocks, b)
		off = 0
	}
	b := a.blocks[len(a.blocks)-1][off : off+n]
	a.used = off + n
	return b
}
func mappedSlice[T any](a *mappedArena, n int) []T {
	if n == 0 {
		return nil
	}
	var v T
	b := a.alloc(n*int(unsafe.Sizeof(v)), int(unsafe.Alignof(v)))
	return unsafe.Slice((*T)(unsafe.Pointer(&b[0])), n)
}
func (a *mappedArena) text(s string) string {
	if s == "" {
		return ""
	}
	b := a.alloc(len(s), 1)
	copy(b, s)
	return unsafe.String(&b[0], len(b))
}
func (a *mappedArena) event(src *event, intern map[string]string) *event {
	dst := &mappedSlice[event](a, 1)[0]
	*dst = *src
	clone := func(p *string) {
		if v, ok := intern[*p]; ok {
			*p = v
		} else {
			v := a.text(*p)
			intern[v] = v
			*p = v
		}
	}
	// High-cardinality content is copied directly; the small interner is scoped
	// to one segment so ingest-sized dictionaries cannot survive query startup.
	for _, p := range []*string{&dst.EventUID, &dst.ItemID, &dst.ToolCallID, &dst.TextPreview, &dst.PayloadJSON, &dst.RepoRelPath} {
		*p = a.text(*p)
	}
	for _, p := range []*string{&dst.SessionID, &dst.SourceName, &dst.Harness, &dst.InferenceProvider, &dst.SourceFile, &dst.EventKind, &dst.ActorKind, &dst.PayloadType, &dst.OpStatus, &dst.ToolName, &dst.ToolPhase, &dst.EndpointKind, &dst.ProjectID, &dst.WorktreeRoot, &dst.Cwd} {
		clone(p)
	}
	if src.IO != nil {
		dst.IO = &mappedSlice[toolIO](a, 1)[0]
		*dst.IO = *src.IO
		for _, p := range []*string{&dst.IO.Input, &dst.IO.InputPreview, &dst.IO.OutputPreview, &dst.IO.Project, &dst.IO.Path, &dst.IO.Root} {
			*p = a.text(*p)
		}
	}
	return dst
}
func (a *mappedArena) close() {
	for _, b := range a.blocks {
		syscall.Munmap(b)
	}
	a.blocks = nil
}
func mapRead(path string) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("empty mapped index")
	}
	return syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
}
func (db *Store) eventByUID(uid string) *event {
	i := sort.Search(len(db.events), func(i int) bool { return db.events[i].EventUID >= uid })
	if i < len(db.events) && db.events[i].EventUID == uid {
		return db.events[i]
	}
	return nil
}

type turnRecord struct {
	key   turnKey
	stats turnStats
}

func (db *Store) turn(sid string, seq uint32) (turnStats, bool) {
	i := sort.Search(len(db.turns), func(i int) bool {
		t := db.turns[i].key
		return t.sessionID > sid || t.sessionID == sid && t.turnSeq >= seq
	})
	if i < len(db.turns) && db.turns[i].key == (turnKey{sid, seq}) {
		return db.turns[i].stats, true
	}
	return turnStats{}, false
}
func freezeSlice[T any](a *mappedArena, old []T) []T {
	out := mappedSlice[T](a, len(old))
	copy(out, old)
	return out
}
func freezePaths(a *mappedArena, p *pathIndex) {
	p.paths = freezeSlice(a, p.paths)
	for i := range p.paths {
		p.paths[i].reversed = a.text(p.paths[i].reversed)
		p.paths[i].docs = freezeSlice(a, p.paths[i].docs)
	}
	p.pathFallback = freezeSlice(a, p.pathFallback)
	p.shellDocs = freezeSlice(a, p.shellDocs)
}
