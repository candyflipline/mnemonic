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
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/arena/b/normalize"
	_ "github.com/candyflipline/mnemonic/arena/b/normalize/all"
	"github.com/candyflipline/mnemonic/arena/b/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

const storeVersion = 3

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
	MaxVersion      uint64
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
	Columns
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
	listRows          []lsItem
	manifest          Manifest
	directory         string
	stamp             manifestStamp
	events            []*event
	arena             mappedArena
	sessions          map[string][]*event
	turns             []turnRecord
	stats             map[string]*sessionStats
	segments          []*segment
	docs, totalLength uint64
	decoder           *zstd.Decoder
	heavyCache        map[heavyKey]heavyBlock
	heavyOrder        []heavyKey
	heavyBytes        int
	pathIndex
	scoreValues    []float64
	scoreStamps    []uint32
	scoreEpoch     uint32
	scoreTouched   []uint32
	postingScratch []posting
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
	shared.ResetAttentionCache()
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	m, e := readManifest(dir)
	if e != nil {
		return e
	}
	changedFormat := m.Version != storeVersion || m.Parser != normalize.Version
	if changedFormat {
		m = Manifest{Version: storeVersion, Parser: normalize.Version, Sources: map[string]Checkpoint{}}
	}
	paths, e := filepath.Glob(filepath.Join(root, "raw", "author=*", "machine=*", "harness=*", "*.json"))
	if e != nil {
		return e
	}
	var codecs []*segmentCodecs
	pool := make(chan *segmentCodecs, 3)
	type pendingSource struct {
		rel    string
		cp     Checkpoint
		writer *sourceStream
	}
	var pending []pendingSource
	defer func() {
		for _, p := range pending {
			p.writer.abort()
		}
		for _, c := range codecs {
			c.close()
		}
	}()
	seen := map[string]bool{}
	dirty := changedFormat
	publish := func() error {
		for _, p := range pending {
			if err := p.writer.wait(); err != nil {
				return err
			}
			p.cp.Events += p.writer.count
			p.cp.MaxVersion = p.writer.maxVersion
			p.cp.Segments = append(p.cp.Segments, p.writer.names...)
			m.Sources[p.rel] = p.cp
		}
		if err := atomicManifest(dir, m); err != nil {
			return err
		}
		pending = pending[:0]
		dirty = false
		return nil
	}
	for _, p := range paths {
		if len(pending) >= 16 {
			if err := publish(); err != nil {
				return err
			}
		}
		rel, e := filepath.Rel(filepath.Join(root, "raw"), p)
		if e != nil {
			return e
		}
		seen[rel] = true
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		var disk struct {
			wire.Source
			ZstSize *int64 `json:"zst_size"`
		}
		if e = json.Unmarshal(b, &disk); e != nil {
			return e
		}
		src := disk.Source
		f, e := os.Open(strings.TrimSuffix(p, ".json") + ".jsonl.zst")
		if e != nil {
			return e
		}
		fi, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		end := fi.Size()
		if disk.ZstSize != nil {
			end = *disk.ZstSize
		}
		if end < 0 || end > fi.Size() || src.Size < 0 {
			f.Close()
			return fmt.Errorf("invalid committed raw size")
		}
		cp, exists := m.Sources[rel]
		resume := exists && cp.Compressed <= end && cp.Source.HeadHash == src.HeadHash && cp.Source.Path == src.Path && cp.Source.Harness == src.Harness && cp.Source.SourceName == src.SourceName
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
		if resume && cp.Compressed == end {
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
		zr, e := zstd.NewReader(bufio.NewReaderSize(io.NewSectionReader(f, cp.Compressed, end-cp.Compressed), 256<<10), zstd.WithDecoderConcurrency(1))
		if e != nil {
			f.Close()
			return e
		}
		if len(codecs) == 0 {
			for i := 0; i < cap(pool); i++ {
				c, err := newSegmentCodecs()
				if err != nil {
					zr.Close()
					f.Close()
					return err
				}
				codecs = append(codecs, c)
				pool <- c
			}
		}
		writer := newSourceStream(dir, pool, cp.MaxVersion)
		state, parseErr := parser.Parse(zr, normalize.SourceMeta{SourceName: src.SourceName, Harness: src.Harness, SourceFile: src.Path}, cp.State, writer.emit)
		zr.Close()
		if parseErr == nil && state.Offset != uint64(src.Size) {
			parseErr = fmt.Errorf("raw size does not match committed manifest")
		}
		if parseErr != nil {
			f.Close()
			writer.abort()
			return parseErr
		}
		hash, e := windowHash(f, end)
		f.Close()
		if e != nil {
			writer.abort()
			return e
		}
		writer.seal()
		parts := strings.Split(filepath.ToSlash(rel), "/")
		cp.Author = strings.TrimPrefix(parts[0], "author=")
		cp.Machine = strings.TrimPrefix(parts[1], "machine=")
		cp.Source = src
		cp.State = state
		cp.Compressed = end
		cp.Window = hash
		pending = append(pending, pendingSource{rel, cp, writer})

	}
	for len(pending) > 0 {
		if err := publish(); err != nil {
			return err
		}
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
		if !live[n] && (strings.HasPrefix(n, "segment-") || strings.HasPrefix(n, ".segment-") || strings.HasPrefix(n, ".manifest-") || strings.HasPrefix(n, ".view-")) {
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

type manifestStamp struct {
	modified int64
	size     int64
}

func stampManifest(dir string) manifestStamp {
	f, e := os.Stat(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return manifestStamp{}
	}
	return manifestStamp{f.ModTime().UnixNano(), f.Size()}
}
func (db *Store) Close() {
	if db == nil {
		return
	}
	for _, sg := range db.segments {
		if sg.postings != nil {
			syscall.Munmap(sg.postings)
		}
		if sg.heavy != nil {
			syscall.Munmap(sg.heavy)
		}
	}
	if db.decoder != nil {
		db.decoder.Close()
	}
	db.arena.close()
}
func loadStore(dir string) (*Store, error) {
	for attempt := 0; ; attempt++ {
		before := stampManifest(dir)
		db, e := loadSnapshot(dir)
		if e == nil {
			db.directory = dir
			db.stamp = before
			return db, nil
		}
		if attempt >= 3 || before == stampManifest(dir) {
			return nil, e
		}
	}
}
func loadSnapshot(dir string) (_ *Store, loadErr error) {
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
	db := &Store{manifest: m, sessions: map[string][]*event{}, stats: map[string]*sessionStats{}, decoder: decoder, heavyCache: map[heavyKey]heavyBlock{}, arena: mappedArena{dir: dir}}
	defer func() {
		if loadErr != nil {
			db.Close()
		}
	}()
	keys := make([]string, 0, len(m.Sources))
	capacity := uint64(0)
	for k, cp := range m.Sources {
		keys = append(keys, k)
		capacity += cp.Events
	}
	sort.Strings(keys)
	all := mappedSlice[*event](&db.arena, int(capacity))
	count := 0
	for _, k := range keys {
		for _, name := range m.Sources[k].Segments {
			sg := &segment{path: filepath.Join(dir, name)}
			db.segments = append(db.segments, sg)
			f, err := os.Open(filepath.Join(sg.path, "meta.zst"))
			if err != nil {
				return nil, err
			}
			zr, err := zstd.NewReader(bufio.NewReaderSize(f, 64<<10), zstd.WithDecoderConcurrency(1))
			if err != nil {
				f.Close()
				return nil, err
			}
			dec := gob.NewDecoder(bufio.NewReader(zr))
			start := count
			intern := map[string]string{}
			for {
				var ev event
				err = dec.Decode(&ev)
				if err == io.EOF {
					break
				}
				if err != nil {
					zr.Close()
					f.Close()
					return nil, err
				}
				if count >= len(all) {
					zr.Close()
					f.Close()
					return nil, fmt.Errorf("metadata exceeds manifest event count")
				}
				ev.segment = sg
				all[count] = db.arena.event(&ev, intern)
				count++
			}
			zr.Close()
			f.Close()
			sg.pending = freezeSlice(&db.arena, all[start:count])
			sg.terms, sg.postings, err = loadTerms(filepath.Join(sg.path, "terms"))
			if err != nil {
				return nil, err
			}
			sg.terms = freezeSlice(&db.arena, sg.terms)
			for i := range sg.terms {
				sg.terms[i].term = db.arena.text(sg.terms[i].term)
			}
			sg.heavy, err = mapRead(filepath.Join(sg.path, "heavy"))
			if err != nil {
				return nil, err
			}
		}
	}
	all = all[:count]
	// Sort stable source order only for equal-version winners, then compact in
	// place. No resident UID hash table or per-event heap allocation is needed.
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.EventUID != b.EventUID {
			return a.EventUID < b.EventUID
		}
		return a.EventVersion < b.EventVersion
	})
	live := 0
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && all[j].EventUID == all[i].EventUID {
			j++
		}
		all[live] = all[j-1]
		live++
		i = j
	}
	db.events = all[:live]
	ordered := freezeSlice(&db.arena, db.events)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		return eventLess(a, b)
	})
	turnRecords := mappedSlice[turnRecord](&db.arena, live)
	turnCount := 0
	for start := 0; start < len(ordered); {
		end := start + 1
		sid := ordered[start].SessionID
		for end < len(ordered) && ordered[end].SessionID == sid {
			end++
		}
		es := ordered[start:end]
		db.sessions[sid] = es
		users := uint32(0)
		ss := &mappedSlice[sessionStats](&db.arena, 1)[0]
		ss.Start = es[0].Time
		ss.End = es[len(es)-1].Time
		for i, ev := range es {
			ev.Order = uint64(i + 1)
			if ev.ActorKind == "user" && ev.EventKind == "message" {
				users++
			}
			ev.Turn = ev.TurnIndex
			if ev.Turn == 0 {
				ev.Turn = max(uint32(1), users)
			}
			if ev.PayloadType == "task_complete" {
				ss.Complete = true
			}
		}
		db.stats[sid] = ss
		start = end
	}
	byTurn := freezeSlice(&db.arena, ordered)
	sort.Slice(byTurn, func(i, j int) bool {
		a, b := byTurn[i], byTurn[j]
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		if a.Turn != b.Turn {
			return a.Turn < b.Turn
		}
		return a.Order < b.Order
	})
	for start := 0; start < len(byTurn); {
		end := start + 1
		first := byTurn[start]
		for end < len(byTurn) && byTurn[end].SessionID == first.SessionID && byTurn[end].Turn == first.Turn {
			end++
		}
		ts := turnStats{Count: int64(end - start)}
		for i := start; i < end; i++ {
			ev := byTurn[i]
			ev.Ordinal = uint32(i - start + 1)
			if ev.PayloadType == "task_complete" {
				ts.Complete = true
			}
			if ev.PayloadType == "task_complete" || ev.PayloadType == "turn_aborted" {
				ts.Terminal = ev.EventUID
			}
		}
		turnRecords[turnCount] = turnRecord{turnKey{first.SessionID, first.Turn}, ts}
		turnCount++
		start = end
	}
	db.turns = turnRecords[:turnCount]
	for _, ev := range db.events {
		if ev.Length > 0 {
			db.docs++
			db.totalLength += uint64(ev.Length)
		}
	}
	for _, sg := range db.segments {
		sg.mapping = mappedSlice[uint32](&db.arena, len(sg.pending))
		for i, ev := range sg.pending {
			j := sort.Search(len(db.events), func(j int) bool { return db.events[j].EventUID >= ev.EventUID })
			if j < len(db.events) && db.events[j] == ev {
				sg.mapping[i] = uint32(j)
			} else {
				sg.mapping[i] = ^uint32(0)
			}
		}
	}
	db.buildPaths()
	db.buildListRollups()
	db.scoreValues = mappedSlice[float64](&db.arena, len(db.events))
	db.scoreStamps = mappedSlice[uint32](&db.arena, len(db.events))
	db.scoreTouched = mappedSlice[uint32](&db.arena, len(db.events))[:0]
	db.postingScratch = mappedSlice[posting](&db.arena, len(db.events))[:0]
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
	return navRow{sessionID: e.SessionID, eventUID: e.EventUID, eventTime: formatRFC3339UTCMillis(e.Time), eventUnixMs: e.Time, sourceFile: e.SourceFile, eventClass: e.EventKind, actorRole: e.ActorKind, payloadType: e.PayloadType, callID: e.ToolCallID, name: e.ToolName, phase: firstNonEmpty(e.ToolPhase, e.OpStatus), itemID: e.ItemID, sourceName: e.SourceName, harness: e.Harness, inferenceProvider: e.InferenceProvider, textContent: e.TextPreview, payloadJSON: e.PayloadJSON, endpointKind: e.EndpointKind, eventOrder: e.Order, turnSeq: e.Turn, eventOrdinal: e.Ordinal}
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
		a, b := db.eventByUID(rows[i].eventUID), db.eventByUID(rows[j].eventUID)
		if a.segment.path != b.segment.path {
			return a.segment.path < b.segment.path
		}
		return a.HeavyOffset < b.HeavyOffset
	})
	for _, r := range rows {
		h, e := db.heavyFor(db.eventByUID(r.eventUID))
		if e != nil {
			return e
		}
		r.textContent = h.Text
		r.payloadJSON = h.Payload
		r.tokenUsageJSON = h.Tokens
		r.tokenUsageBuckets = h.Buckets
		r.tokenUsageNative = h.Native
	}
	return nil
}
