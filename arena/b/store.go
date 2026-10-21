package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/normalize"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

const storeVersion = 2

// CONTRACT-NOTES requires a deterministic stamp after all real records.
// It affects ordering; oracle wall-clock values at these positions are masked.
const fallbackEventMillis int64 = 253402300799999 // 9999-12-31T23:59:59.999Z

type Checkpoint struct {
	Source          wire.Source
	Author, Machine string
	Compressed      int64
	Window          string
	State           normalize.State
	Segments        []string
	Events          uint64
}
type Manifest struct {
	Version, Parser int
	Sources         map[string]Checkpoint
}
type event struct {
	TextHash    uint64
	HeavyOffset uint64
	HeavyIndex  uint32
	segment     *segment
	schema.Event
	Time, Timestamp int64
	Emission        uint32
	Order           uint64
	Turn, Ordinal   uint32
	Length          uint32
	Flags           uint16
	IO              *toolIO
}
type toolIO struct {
	Input         string `json:"input_json"`
	InputPreview  string `json:"input_preview"`
	OutputPreview string `json:"output_preview"`
	Project       string `json:"project_id"`
	Path          string `json:"repo_rel_path"`
	Root          string `json:"worktree_root"`
}
type posting struct{ Doc, TF uint32 }
type turnStats struct {
	Count    int64
	Complete bool
	Terminal string
}
type sessionStats struct {
	Start, End int64
	Complete   bool
}
type Store struct {
	manifest          Manifest
	events            []*event
	byUID             map[string]*event
	sessions          map[string][]*event
	turns             map[turnKey]turnStats
	stats             map[string]sessionStats
	segments          []*segment
	docs, totalLength uint64
	decoder           *zstd.Decoder
	heavyCache        map[heavyKey]heavyBlock
	heavyOrder        []heavyKey
	heavyBytes        int
}
type turnKey struct {
	sessionID string
	turnSeq   uint32
}

func readManifest(dir string) (Manifest, error) {
	m := Manifest{Version: storeVersion, Parser: normalize.Version, Sources: map[string]Checkpoint{}}
	b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if errors.Is(e, os.ErrNotExist) {
		return m, nil
	}
	if e != nil {
		return m, e
	}
	e = json.Unmarshal(b, &m)
	return m, e
}
func syncDir(dir string) error {
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func atomicManifest(dir string, m Manifest) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".manifest-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, filepath.Join(dir, "manifest.json")); e != nil {
		return e
	}
	return syncDir(dir)
}
func windowHash(f *os.File, end int64) (string, error) {
	start := max(int64(0), end-(64<<10))
	h := sha256.New()
	_, e := io.Copy(h, io.NewSectionReader(f, start, end-start))
	return hex.EncodeToString(h.Sum(nil)), e
}
func ingest(root, dir string) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	m, e := readManifest(dir)
	if e != nil {
		return e
	}
	if m.Version != storeVersion || m.Parser != normalize.Version {
		m = Manifest{Version: storeVersion, Parser: normalize.Version, Sources: map[string]Checkpoint{}}
	}
	paths, e := filepath.Glob(filepath.Join(root, "raw", "author=*", "machine=*", "harness=*", "*.json"))
	if e != nil {
		return e
	}
	seen := map[string]bool{}
	dirty := false
	for _, p := range paths {
		rel, e := filepath.Rel(filepath.Join(root, "raw"), p)
		if e != nil {
			return e
		}
		seen[rel] = true
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		var src wire.Source
		if e = json.Unmarshal(b, &src); e != nil {
			return e
		}
		f, e := os.Open(strings.TrimSuffix(p, ".json") + ".jsonl.zst")
		if e != nil {
			return e
		}
		fi, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		cp := m.Sources[rel]
		resume := cp.Compressed > 0 && cp.Compressed <= fi.Size() && cp.Source.HeadHash == src.HeadHash && cp.Source.Path == src.Path && cp.Source.Harness == src.Harness && cp.Source.SourceName == src.SourceName
		if resume {
			h, err := windowHash(f, cp.Compressed)
			if err != nil {
				f.Close()
				return err
			}
			resume = h == cp.Window
		}
		if !resume {
			cp = Checkpoint{}
		}
		if resume && cp.Compressed == fi.Size() {
			f.Close()
			if cp.Source != src {
				cp.Source = src
				m.Sources[rel] = cp
				dirty = true
			}
			continue
		}
		parser, ok := normalize.For(src.Harness)
		if !ok {
			f.Close()
			return fmt.Errorf("unsupported harness %q", src.Harness)
		}
		zr, e := zstd.NewReader(io.NewSectionReader(f, cp.Compressed, fi.Size()-cp.Compressed), zstd.WithDecoderConcurrency(1))
		if e != nil {
			f.Close()
			return e
		}
		writer, e := newSegmentWriter(dir)
		if e != nil {
			zr.Close()
			f.Close()
			return e
		}
		state, parseErr := parser.Parse(zr, normalize.SourceMeta{SourceName: src.SourceName, Harness: src.Harness, SourceFile: src.Path}, cp.State, writer.emit)
		zr.Close()
		if parseErr != nil {
			f.Close()
			writer.abort()
			return parseErr
		}
		hash, e := windowHash(f, fi.Size())
		f.Close()
		if e != nil {
			writer.abort()
			return e
		}
		name, e := writer.finish()
		if e != nil {
			return e
		}
		count := uint64(writer.count)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		cp.Author = strings.TrimPrefix(parts[0], "author=")
		cp.Machine = strings.TrimPrefix(parts[1], "machine=")
		cp.Source = src
		cp.State = state
		cp.Compressed = fi.Size()
		cp.Window = hash
		cp.Events += count
		cp.Segments = append(cp.Segments, name)
		m.Sources[rel] = cp
		if e = atomicManifest(dir, m); e != nil {
			return e
		}
		dirty = false
	}
	for k := range m.Sources {
		if !seen[k] {
			delete(m.Sources, k)
			dirty = true
		}
	}
	if dirty {
		if e = atomicManifest(dir, m); e != nil {
			return e
		}
	}
	// Reclaim only files not reachable from the last durable manifest.
	live := map[string]bool{"manifest.json": true}
	for _, cp := range m.Sources {
		for _, s := range cp.Segments {
			live[s] = true
		}
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, ent := range entries {
		n := ent.Name()
		if !live[n] && (strings.HasPrefix(n, "segment-") || strings.HasPrefix(n, ".segment-") || strings.HasPrefix(n, ".manifest-")) {
			if e = os.RemoveAll(filepath.Join(dir, n)); e != nil {
				return e
			}
		}
	}
	return nil
}

func parseTime(s string, fallback int64) int64 {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.UnixMilli()
		}
	}
	return fallback
}
func loadStore(dir string) (*Store, error) {
	m, e := readManifest(dir)
	if e != nil {
		return nil, e
	}
	if m.Version != storeVersion || m.Parser != normalize.Version {
		return nil, fmt.Errorf("store needs ingest with current parser")
	}
	decoder, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if e != nil {
		return nil, e
	}
	db := &Store{manifest: m, byUID: map[string]*event{}, sessions: map[string][]*event{}, turns: map[turnKey]turnStats{}, stats: map[string]sessionStats{}, decoder: decoder, heavyCache: map[heavyKey]heavyBlock{}}
	keys := make([]string, 0, len(m.Sources))
	for k := range m.Sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	interned := map[string]string{}
	intern := func(s string) string {
		if v, ok := interned[s]; ok {
			return v
		}
		interned[s] = s
		return s
	}
	for _, k := range keys {
		cp := m.Sources[k]
		for _, name := range cp.Segments {
			sg := &segment{path: filepath.Join(dir, name)}
			f, e := os.Open(filepath.Join(sg.path, "meta.zst"))
			if e != nil {
				return nil, e
			}
			zr, e := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
			if e != nil {
				f.Close()
				return nil, e
			}
			dec := gob.NewDecoder(bufio.NewReader(zr))
			for {
				ev := new(event)
				e = dec.Decode(ev)
				if e == io.EOF {
					break
				}
				if e != nil {
					zr.Close()
					f.Close()
					return nil, e
				}
				ev.segment = sg
				ev.Time = parseTime(ev.RecordTS, fallbackEventMillis)
				ev.Timestamp = parseTime(ev.EventTS, 0)
				for _, p := range []*string{&ev.SessionID, &ev.SourceName, &ev.Harness, &ev.InferenceProvider, &ev.SourceFile, &ev.EventKind, &ev.ActorKind, &ev.PayloadType, &ev.OpStatus, &ev.ToolName, &ev.Cwd, &ev.ProjectID, &ev.WorktreeRoot, &ev.EndpointKind} {
					*p = intern(*p)
				}
				sg.pending = append(sg.pending, ev)
				if prev := db.byUID[ev.EventUID]; prev == nil || prev.EventVersion <= ev.EventVersion {
					db.byUID[ev.EventUID] = ev
				}
			}
			zr.Close()
			f.Close()
			sg.terms, sg.postings, e = loadTerms(filepath.Join(sg.path, "terms"))
			if e != nil {
				return nil, e
			}
			db.segments = append(db.segments, sg)
		}
	}
	for _, ev := range db.byUID {
		db.sessions[ev.SessionID] = append(db.sessions[ev.SessionID], ev)
	}
	for sid, es := range db.sessions {
		sort.Slice(es, func(i, j int) bool { return eventLess(es[i], es[j]) })
		users := uint32(0)
		ord := map[uint32]uint32{}
		ss := sessionStats{Start: es[0].Time, End: es[len(es)-1].Time}
		for i, ev := range es {
			ev.Order = uint64(i + 1)
			if ev.ActorKind == "user" && ev.EventKind == "message" {
				users++
			}
			ev.Turn = ev.TurnIndex
			if ev.Turn == 0 {
				ev.Turn = max(uint32(1), users)
			}
			ord[ev.Turn]++
			ev.Ordinal = ord[ev.Turn]
			tk := turnKey{sid, ev.Turn}
			ts := db.turns[tk]
			ts.Count++
			if ev.PayloadType == "task_complete" {
				ts.Complete = true
				ss.Complete = true
			}
			if ev.PayloadType == "task_complete" || ev.PayloadType == "turn_aborted" {
				ts.Terminal = ev.EventUID
			}
			db.turns[tk] = ts
		}
		db.stats[sid] = ss
	}
	// Document IDs are canonical and deterministic independently of map iteration.
	for _, es := range db.sessions {
		db.events = append(db.events, es...)
	}
	sort.Slice(db.events, func(i, j int) bool { return db.events[i].EventUID < db.events[j].EventUID })
	global := make(map[*event]uint32, len(db.events))
	for i, ev := range db.events {
		global[ev] = uint32(i)
		if ev.Length > 0 {
			db.docs++
			db.totalLength += uint64(ev.Length)
		}
	}
	for _, sg := range db.segments {
		sg.mapping = make([]uint32, len(sg.pending))
		for i, ev := range sg.pending {
			if id, ok := global[ev]; ok {
				sg.mapping[i] = id
			} else {
				sg.mapping[i] = ^uint32(0)
			}
		}
		sg.pending = nil
	}
	return db, nil
}
func eventLess(a, b *event) bool {
	if a.Time != b.Time {
		return a.Time < b.Time
	}
	if a.SourceFile != b.SourceFile {
		return a.SourceFile < b.SourceFile
	}
	if a.SourceGeneration != b.SourceGeneration {
		return a.SourceGeneration < b.SourceGeneration
	}
	if a.SourceOffset != b.SourceOffset {
		return a.SourceOffset < b.SourceOffset
	}
	if a.SourceLineNo != b.SourceLineNo {
		return a.SourceLineNo < b.SourceLineNo
	}
	if a.Emission != b.Emission {
		return a.Emission < b.Emission
	}
	return a.EventUID < b.EventUID
}
func (db *Store) nav(e *event) navRow {
	return navRow{sessionID: e.SessionID, eventUID: e.EventUID, eventTime: formatRFC3339UTCMillis(e.Time), eventUnixMs: e.Time, sourceFile: e.SourceFile, sourceRef: e.SourceRef, eventClass: e.EventKind, actorRole: e.ActorKind, payloadType: e.PayloadType, callID: e.ToolCallID, name: e.ToolName, phase: firstNonEmpty(e.ToolPhase, e.OpStatus), itemID: e.ItemID, sourceName: e.SourceName, harness: e.Harness, inferenceProvider: e.InferenceProvider, textContent: e.TextPreview, payloadJSON: e.PayloadJSON, tokenUsageJSON: e.TokenUsageJSON, tokenUsageBuckets: e.TokenUsageBuckets, tokenUsageNative: e.TokenUsageNativeUnits, endpointKind: e.EndpointKind, eventOrder: e.Order, turnSeq: e.Turn, eventOrdinal: e.Ordinal}
}
func hydrateKept(_ context.Context, db *Store, sc *sessionScan) error {
	var rows []*navRow
	if sc.target != nil {
		rows = append(rows, sc.target)
	}
	for _, t := range sc.turns {
		for _, r := range []*navRow{t.userInput, t.finalResponse} {
			if r != nil {
				rows = append(rows, r)
			}
		}
		rows = append(rows, t.selected...)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := db.byUID[rows[i].eventUID], db.byUID[rows[j].eventUID]
		if a.segment.path != b.segment.path {
			return a.segment.path < b.segment.path
		}
		return a.HeavyOffset < b.HeavyOffset
	})
	for _, r := range rows {
		h, e := db.heavyFor(db.byUID[r.eventUID])
		if e != nil {
			return e
		}
		r.textContent = h.Text
		r.payloadJSON = h.Payload
		r.tokenUsageJSON = h.Tokens
	}
	return nil
}
