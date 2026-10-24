package index

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/parquet-go/parquet-go"
)

// KindArchive is a ch-* parquet file the retired 59796bc import-ch wrote
// (lake shape, machine partition archive-ch), read in place. The kind and
// its three functions live here; kind.go is not touched — nothing above
// ingest looks at the kind. The source granularity is (file, session): one
// pass over a changed file streams its rows into per-session members, so
// components stay session-sized exactly as for raw and an unchanged file is
// skipped after reading only its size and last window (DESIGN.md, Archive
// kind).
const KindArchive Kind = 2

// archiveMemberSep joins a file's source key and a session id into a
// member's source key. Session ids are uuids, which never carry it.
const archiveMemberSep = "~"

// archiveOpenLogs bounds the open frame-log pairs while rows stream: a
// ch-* file holds up to ~2,800 sessions and its rows are not sorted by
// session, so the least recently used member is flushed and closed and
// reopens at its recorded lengths if a later row needs it.
const archiveOpenLogs = 64

// archiveReadRows is the row slice one row group decodes into at a time.
const archiveReadRows = 512

// listArchiveSources is every ch-* file under root's events tree, one file
// source of KindArchive per path (import-ch writes nowhere else than
// machine=archive-ch). The manifest carries the file's own size and mtime;
// the per-session members are discovered from the file's checkpoint, not by
// reading the parquet.
func listArchiveSources(root string) ([]source, error) {
	paths, err := filepath.Glob(filepath.Join(root, "events", "author=*",
		"machine="+layout.ArchiveMachine, "harness=*", "ch-*.parquet"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]source, 0, len(paths))
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		rel, err := filepath.Rel(filepath.Join(root, "events"), p)
		if err != nil {
			return nil, err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 4 {
			continue
		}
		harness := strings.TrimPrefix(parts[2], "harness=")
		srcID := strings.TrimSuffix(parts[3], ".parquet")
		out = append(out, source{
			kind:  KindArchive,
			mpath: p,
			key: layout.Key{
				Author:  strings.TrimPrefix(parts[0], "author="),
				Machine: layout.ArchiveMachine,
				Harness: harness,
				SrcID:   srcID,
			},
			src: manifest{Source: wire.Source{
				SrcID: srcID, Harness: harness,
				Path: filepath.ToSlash(p), Size: fi.Size(), UpdatedMs: fi.ModTime().UnixMilli(),
			}},
		})
	}
	return out, nil
}

// archiveMemberKey is a member's source key: one session of one file.
func archiveMemberKey(fileKey, sessionID string) string {
	return fileKey + archiveMemberSep + sessionID
}

// memberBeats reports whether source c's copy of a uid beats the incumbent
// w at the cross-source compare: a raw-derived member beats an archive
// member unconditionally — the cache's winner order hands ties to the
// non-archive copy (its index.go winner row_number) and an archive file is
// never re-parsed, so its event_version only ages; within a kind the
// greater lake path wins, the same order the cache uses across lake files.
func memberBeats(c ckpt, wKind Kind, wPath string) bool {
	if (c.Kind == KindArchive) != (wKind == KindArchive) {
		return wKind == KindArchive
	}
	return c.Path > wPath
}

// ingestArchive brings one ch-* file's per-session members up to the file.
// The identity is the file's size and last window (a parquet file is never
// appended, so the kind's byte test is same size AND same window): an
// unchanged file is skipped without opening the parquet, a changed one is
// re-parsed in one pass whose rows route to per-session members, and the
// file's checkpoint — which names the member sessions publish expands — is
// written last. A member's checkpoint carries the parent file's identity,
// so parts fingerprint exactly as one member per session.
func ingestArchive(store string, s source) (Outcome, int64, error) {
	key := sourceKey(s.key)
	f, err := os.Open(s.mpath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	end := fi.Size()
	sum, err := windowSHA(f, end)
	if err != nil {
		return 0, 0, err
	}
	prev, has := readCkpt(store, key)
	if has && prev.Kind == KindArchive && prev.ZstSize == end && prev.WindowSHA == sum {
		return Unchanged, 0, nil
	}
	pf, err := parquet.OpenFile(f, end)
	if err != nil {
		return 0, 0, err
	}
	prevGen := map[string]int{}
	for _, sid := range prev.Sessions {
		if c, ok := readCkpt(store, archiveMemberKey(key, sid)); ok {
			prevGen[sid] = c.Gen
		}
	}
	ws := &archiveWriters{store: store, fileKey: key, prevGen: prevGen,
		ws: map[string]*archiveWriter{}, limit: archiveOpenLogs}
	if err := streamArchiveRows(pf, ws); err != nil {
		return 0, 0, err
	}
	if err := ws.closeAll(); err != nil {
		return 0, 0, err
	}
	// Member checkpoints after their logs are synced (closeAll), members
	// the file lost deleted, the file checkpoint last: the same
	// checkpoint-after-logs order the raw path keeps, so the file
	// checkpoint never names a member whose rows are not durable.
	written, rows := int64(0), int64(0)
	sessions := make([]string, 0, len(ws.ws))
	for sid, w := range ws.ws {
		mc := w.ckpt(s.key, s.src, end, sum)
		b, _ := json.Marshal(mc)
		if err := writeFileAtomic(ckptPath(store, w.key), b); err != nil {
			return 0, 0, err
		}
		written += w.lightLen + w.heavyLen
		rows += w.rows
		sessions = append(sessions, sid)
	}
	sort.Strings(sessions)
	for _, sid := range prev.Sessions {
		if _, ok := ws.ws[sid]; ok {
			continue
		}
		if err := removeMember(store, archiveMemberKey(key, sid)); err != nil {
			return 0, 0, err
		}
	}
	want := wantArchiveCkpt(s, end, sum, rows, sessions)
	b, _ := json.Marshal(want)
	if err := writeFileAtomic(ckptPath(store, key), b); err != nil {
		return 0, 0, err
	}
	if has {
		return Reparsed, written, nil
	}
	return Fresh, written, nil
}

// streamArchiveRows reads every row group into a bounded slice and routes
// each row to its session's member: the file is never held whole, one row
// group's columns plus the slice is the peak.
func streamArchiveRows(pf *parquet.File, ws *archiveWriters) error {
	buf := make([]chRow, archiveReadRows)
	for _, rg := range pf.RowGroups() {
		rd := parquet.NewGenericRowGroupReader[chRow](rg)
		for {
			n, err := rd.Read(buf)
			for i := 0; i < n; i++ {
				if err := ws.add(&buf[i]); err != nil {
					rd.Close()
					return err
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				rd.Close()
				return err
			}
		}
		rd.Close()
	}
	return nil
}

// wantArchiveCkpt is the file-level checkpoint: it names the member
// sessions, carries the file identity for the unchanged test, and never
// joins planning itself (publish expands it into the members).
func wantArchiveCkpt(s source, size int64, sum string, rows int64, sessions []string) ckpt {
	return ckpt{parsed: parsed{
		Key: sourceKey(s.key), Kind: KindArchive, Layout: logLayout,
		Harness: s.src.Harness, SourceFile: s.src.Path,
		Author: s.key.Author, Machine: s.key.Machine,
		Path:    filepath.ToSlash(s.key.Events("")),
		ZstSize: size, WindowSHA: sum,
		Rows: rows, Sessions: sessions,
	}, ColdZst: size}
}

// removeMember drops a session's member the file no longer holds: its
// checkpoint and the log generation that checkpoint named. Anything left
// half-written by a killed run goes with gcLogs.
func removeMember(store, key string) error {
	if c, ok := readCkpt(store, key); ok {
		for _, kind := range []string{"light", "heavy"} {
			if err := os.Remove(logPath(store, key, c.Gen, kind)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return os.Remove(ckptPath(store, key))
}

// archiveWriter is one session's frame logs while a file parses.
type archiveWriter struct {
	key      string
	gen      int
	lf, hf   *os.File
	lw, hw   *frameWriter
	isOpen   bool
	rows     int64
	lightLen int64
	heavyLen int64
}

// ckpt is the member checkpoint: the parent file's identity (size and
// window) and the member's own rows, so parts fingerprint per session and
// an unchanged file rewrites nothing.
func (w *archiveWriter) ckpt(k layout.Key, m manifest, size int64, sha string) ckpt {
	return ckpt{parsed: parsed{
		Key: w.key, Kind: KindArchive, Layout: logLayout,
		Harness: m.Harness, SourceFile: m.Path,
		Author: k.Author, Machine: k.Machine,
		Path: filepath.ToSlash(k.Events("")),
		// Only the member's own session: the component bound the parts
		// planner joins on.
		Sessions: []string{strings.TrimPrefix(w.key, sourceKey(k)+archiveMemberSep)},
		ZstSize:  size, WindowSHA: sha,
		Rows: w.rows,
	}, Gen: w.gen, LightLen: w.lightLen, HeavyLen: w.heavyLen}
}

// archiveWriters holds the members a parse has touched, at most limit with
// open logs. Rows are not sorted by session, so the least recently used
// member's logs are flushed and closed on open of a new one; reopening at
// the recorded lengths appends valid frames, since a frame's place is fixed
// when its records are added and only moves on flush.
type archiveWriters struct {
	store   string
	fileKey string
	prevGen map[string]int
	ws      map[string]*archiveWriter
	opened  []string // member session ids of open writers, least recent first
	limit   int
}

func (aws *archiveWriters) add(r *chRow) error {
	ev := r.event()
	row := rowFrom(&ev)
	// The ch file carries event_ts as micros directly, the same value the
	// raw path derives from the event_ts string (rowFrom's floorDiv(us,
	// 1000)); record_ts went through rowFrom unchanged.
	row.TsMs = floorDiv(r.EventTS, 1000)
	w, err := aws.writerFor(row.Session)
	if err != nil {
		return err
	}
	heavy := appendStr(nil, r.PayloadJSON)
	heavy = appendStr(heavy, r.TokenUsageJSON)
	fo, idx, err := w.hw.add(heavy)
	if err != nil {
		return err
	}
	row.HeavyFrame, row.HeavyIdx = uint64(fo), uint64(idx)
	if _, _, err := w.lw.add(row.append(nil)); err != nil {
		return err
	}
	w.rows++
	return nil
}

func (aws *archiveWriters) writerFor(sid string) (*archiveWriter, error) {
	w, ok := aws.ws[sid]
	if ok {
		if w.isOpen {
			aws.touch(sid)
			return w, nil
		}
		// Reopened after an eviction: the same generation's logs, appended
		// at the recorded lengths. The gen bump is per parse run, not per
		// open.
	} else {
		gen, seen := aws.prevGen[sid]
		if !seen {
			gen = -1
		}
		w = &archiveWriter{key: archiveMemberKey(aws.fileKey, sid), gen: gen + 1}
		aws.ws[sid] = w
	}
	if len(aws.opened) >= aws.limit {
		if err := aws.closeWriter(aws.ws[aws.opened[0]]); err != nil {
			return nil, err
		}
		aws.opened = aws.opened[1:]
	}
	var err error
	w.lf, err = openLog(logPath(aws.store, w.key, w.gen, "light"), w.lightLen)
	if err != nil {
		return nil, err
	}
	w.hf, err = openLog(logPath(aws.store, w.key, w.gen, "heavy"), w.heavyLen)
	if err != nil {
		w.lf.Close()
		return nil, err
	}
	w.lw = &frameWriter{f: w.lf, off: w.lightLen, target: lightFrame, enc: encoder}
	w.hw = &frameWriter{f: w.hf, off: w.heavyLen, target: heavyFrame, enc: heavyEncoder}
	w.isOpen = true
	aws.opened = append(aws.opened, sid)
	return w, nil
}

// touch moves sid to the most recent end of the LRU list.
func (aws *archiveWriters) touch(sid string) {
	for i, s := range aws.opened {
		if s == sid {
			copy(aws.opened[i:], aws.opened[i+1:])
			aws.opened[len(aws.opened)-1] = sid
			return
		}
	}
}

// closeWriter flushes and syncs one member's logs and records their
// lengths; heavy first, the order the raw path keeps.
func (aws *archiveWriters) closeWriter(w *archiveWriter) error {
	if !w.isOpen {
		return nil
	}
	if err := w.hw.flush(); err != nil {
		return err
	}
	if err := w.lw.flush(); err != nil {
		return err
	}
	if err := w.hf.Sync(); err != nil {
		return err
	}
	if err := w.lf.Sync(); err != nil {
		return err
	}
	w.heavyLen, w.lightLen = w.hw.off, w.lw.off
	if err := w.hf.Close(); err != nil {
		return err
	}
	if err := w.lf.Close(); err != nil {
		return err
	}
	w.isOpen = false
	return nil
}

func (aws *archiveWriters) closeAll() error {
	for len(aws.opened) > 0 {
		if err := aws.closeWriter(aws.ws[aws.opened[0]]); err != nil {
			return err
		}
		aws.opened = aws.opened[1:]
	}
	return nil
}

// chRow is one row of a ch-* file, mirroring the 60 columns import-ch
// wrote (schema.EventColumns plus the literal author, machine_id and
// ingested_at; machine is only the hive partition). The types are the
// ClickHouse column types' unsigned widths, the probe's decode shapes: a
// DATE reads as int32 days, event_ts and ingested_at as int64 micros not
// UTC-adjusted, content_types as the standard 3-level list (the ,list
// tag — Go slices default to the legacy repeated encoding DuckDB does not
// write), and the two MAPs by name. rowFrom's row uses the subset the
// engine's rows carry; the file's extra columns decode and drop.
type chRow struct {
	EventUID              string             `parquet:"event_uid"`
	SessionID             string             `parquet:"session_id"`
	SessionDate           int32              `parquet:"session_date,date"`
	SourceName            string             `parquet:"source_name"`
	Harness               string             `parquet:"harness"`
	InferenceProvider     string             `parquet:"inference_provider"`
	SourceFile            string             `parquet:"source_file"`
	SourceInode           uint64             `parquet:"source_inode"`
	SourceGeneration      uint32             `parquet:"source_generation"`
	SourceLineNo          uint64             `parquet:"source_line_no"`
	SourceOffset          uint64             `parquet:"source_offset"`
	SourceRef             string             `parquet:"source_ref"`
	RecordTS              string             `parquet:"record_ts"`
	EventTS               int64              `parquet:"event_ts,timestamp(microsecond)"`
	EventKind             string             `parquet:"event_kind"`
	ActorKind             string             `parquet:"actor_kind"`
	PayloadType           string             `parquet:"payload_type"`
	OpKind                string             `parquet:"op_kind"`
	OpStatus              string             `parquet:"op_status"`
	RequestID             string             `parquet:"request_id"`
	TraceID               string             `parquet:"trace_id"`
	TurnIndex             uint32             `parquet:"turn_index"`
	ItemID                string             `parquet:"item_id"`
	ToolCallID            string             `parquet:"tool_call_id"`
	ParentToolCallID      string             `parquet:"parent_tool_call_id"`
	OriginEventID         string             `parquet:"origin_event_id"`
	OriginToolCallID      string             `parquet:"origin_tool_call_id"`
	ToolName              string             `parquet:"tool_name"`
	ToolPhase             string             `parquet:"tool_phase"`
	ToolError             uint8              `parquet:"tool_error"`
	AgentRunID            string             `parquet:"agent_run_id"`
	AgentLabel            string             `parquet:"agent_label"`
	CoordGroupID          string             `parquet:"coord_group_id"`
	CoordGroupLabel       string             `parquet:"coord_group_label"`
	IsSubstream           uint8              `parquet:"is_substream"`
	Model                 string             `parquet:"model"`
	EndpointKind          string             `parquet:"endpoint_kind"`
	InputTokens           uint32             `parquet:"input_tokens"`
	OutputTokens          uint32             `parquet:"output_tokens"`
	CacheReadTokens       uint32             `parquet:"cache_read_tokens"`
	CacheWriteTokens      uint32             `parquet:"cache_write_tokens"`
	LatencyMs             uint32             `parquet:"latency_ms"`
	RetryCount            uint16             `parquet:"retry_count"`
	ServiceTier           string             `parquet:"service_tier"`
	ContentTypes          []string           `parquet:"content_types,list"`
	HasReasoning          uint8              `parquet:"has_reasoning"`
	TextContent           string             `parquet:"text_content"`
	TextPreview           string             `parquet:"text_preview"`
	PayloadJSON           string             `parquet:"payload_json"`
	TokenUsageJSON        string             `parquet:"token_usage_json"`
	TokenUsageBuckets     map[string]uint64  `parquet:"token_usage_buckets"`
	TokenUsageNativeUnits map[string]float64 `parquet:"token_usage_native_units"`
	ProjectID             string             `parquet:"project_id"`
	RepoRelPath           string             `parquet:"repo_rel_path"`
	WorktreeRoot          string             `parquet:"worktree_root"`
	EventVersion          uint64             `parquet:"event_version"`
	Cwd                   string             `parquet:"cwd"`
	Author                string             `parquet:"author"`
	MachineID             string             `parquet:"machine_id"`
	IngestedAt            int64              `parquet:"ingested_at,timestamp(microsecond)"`
}

// event is the schema.Event rowFrom derives the row from: only the columns
// the engine's rows carry are copied, the same set the raw path fills.
func (r *chRow) event() schema.Event {
	return schema.Event{
		EventUID:              r.EventUID,
		SessionID:             r.SessionID,
		SourceName:            r.SourceName,
		Harness:               r.Harness,
		InferenceProvider:     r.InferenceProvider,
		SourceFile:            r.SourceFile,
		SourceGeneration:      r.SourceGeneration,
		SourceLineNo:          r.SourceLineNo,
		SourceOffset:          r.SourceOffset,
		SourceRef:             r.SourceRef,
		RecordTS:              r.RecordTS,
		EventKind:             r.EventKind,
		ActorKind:             r.ActorKind,
		PayloadType:           r.PayloadType,
		OpKind:                r.OpKind,
		OpStatus:              r.OpStatus,
		RequestID:             r.RequestID,
		TraceID:               r.TraceID,
		TurnIndex:             r.TurnIndex,
		ItemID:                r.ItemID,
		ToolCallID:            r.ToolCallID,
		ParentToolCallID:      r.ParentToolCallID,
		OriginEventID:         r.OriginEventID,
		ToolName:              r.ToolName,
		ToolPhase:             r.ToolPhase,
		ToolError:             r.ToolError,
		AgentRunID:            r.AgentRunID,
		AgentLabel:            r.AgentLabel,
		IsSubstream:           r.IsSubstream,
		Model:                 r.Model,
		EndpointKind:          r.EndpointKind,
		InputTokens:           r.InputTokens,
		OutputTokens:          r.OutputTokens,
		CacheReadTokens:       r.CacheReadTokens,
		CacheWriteTokens:      r.CacheWriteTokens,
		LatencyMs:             r.LatencyMs,
		TextContent:           r.TextContent,
		TextPreview:           r.TextPreview,
		PayloadJSON:           r.PayloadJSON,
		TokenUsageJSON:        r.TokenUsageJSON,
		TokenUsageBuckets:     r.TokenUsageBuckets,
		TokenUsageNativeUnits: r.TokenUsageNativeUnits,
		ProjectID:             r.ProjectID,
		RepoRelPath:           r.RepoRelPath,
		WorktreeRoot:          r.WorktreeRoot,
		EventVersion:          r.EventVersion,
		Cwd:                   r.Cwd,
	}
}
