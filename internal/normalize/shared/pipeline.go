package shared

import (
	"strings"
	"time"
)

// Source is one harness's parsing behavior, ported from upstream
// IngestSource (crates/moraine-ingest-core/src/sources/mod.rs). NormalizeRecord
// runs the shared pipeline around it; parser packages implement the hooks.
type Source interface {
	Harness() string
	DefaultInferenceProvider() string
	// Preflight returns the record to parse and false when it is skipped.
	Preflight(rec JSON) (JSON, bool)
	SourceMetadata(rec JSON) SourceMetadata
	RecordTS(rec JSON) string
	TopType(rec JSON) string
	Cwd(rec JSON) string
	SessionID(rec JSON, sctx SourceRecordContext) string
	Normalize(rec JSON, ctx *Ctx, topType, baseUID, modelHint string) *Partials
}

// SourceMetadata is source_metadata: the record's inference provider plus an
// optional fallback for the session model hint.
type SourceMetadata struct {
	InferenceProvider string
	ModelHintFallback string
}

// DefaultSourceMetadata is the metadata most sources return.
func DefaultSourceMetadata(provider string) SourceMetadata {
	return SourceMetadata{InferenceProvider: provider}
}

// SourceRecordContext is what SessionID resolution sees.
type SourceRecordContext struct {
	SourceName  string
	SourceFile  string
	SessionHint string
	TopType     string
	BaseUID     string
}

// RecordParams is one JSONL line's coordinates plus the hints threaded from
// the previous line. Hints never leak past the file being parsed.
type RecordParams struct {
	SourceName       string
	SourceFile       string
	SourceInode      uint64
	SourceGeneration uint32
	SourceLineNo     uint64
	SourceOffset     uint64
	SessionHint      string
	ModelHint        string
	CwdHint          string
	RecordTSHint     string
}

// NormalizedRecord is one line's outcome: its finalized event rows and the
// hints the next line inherits.
type NormalizedRecord struct {
	Events      []*EventRow
	SessionHint string
	ModelHint   string
	CwdHint     string
}

// NormalizeRecord ports normalize_record_with_ts_hint: resolve the record's
// coordinates and identity, run the source adapter, then fold tool payloads
// into their events and re-key events to their canonical semantic uid.
func NormalizeRecord(rec JSON, src Source, p RecordParams) NormalizedRecord {
	kept, keep := src.Preflight(rec)
	if !keep {
		return NormalizedRecord{}
	}
	rec = kept
	harness := src.Harness()
	meta := src.SourceMetadata(rec)
	recordTS := ResolveRecordTS(harness, p.SourceFile, src.RecordTS(rec), p.RecordTSHint)
	eventTS, tsFailed := ParseEventTS(recordTS)
	_ = tsFailed // error rows are not kept in this port
	topType := src.TopType(rec)
	sessionDate := InferSessionDateFromFile(p.SourceFile, recordTS)
	rawJSON := Compact(rec)
	baseUID := EventUID(p.SourceFile, p.SourceGeneration, p.SourceLineNo, p.SourceOffset, rawJSON, "raw")
	sessionID := src.SessionID(rec, SourceRecordContext{
		SourceName:  p.SourceName,
		SourceFile:  p.SourceFile,
		SessionHint: p.SessionHint,
		TopType:     topType,
		BaseUID:     baseUID,
	})
	cwd := ResolveCwd(src.Cwd(rec), p.CwdHint)
	ctx := &Ctx{
		SourceName:        p.SourceName,
		Harness:           harness,
		InferenceProvider: meta.InferenceProvider,
		SessionID:         sessionID,
		SessionHint:       p.SessionHint,
		SessionDate:       sessionDate,
		Cwd:               cwd,
		SourceFile:        p.SourceFile,
		SourceInode:       p.SourceInode,
		SourceGeneration:  p.SourceGeneration,
		SourceLineNo:      p.SourceLineNo,
		SourceOffset:      p.SourceOffset,
		RecordTS:          recordTS,
		EventTS:           eventTS,
	}
	partials := src.Normalize(rec, ctx, topType, baseUID, p.ModelHint)
	if partials == nil {
		partials = &Partials{}
	}
	folded := FoldToolPayloads(partials)
	StampDuplicateSemanticOccurrences(partials.Events, folded)
	FinalizeIdentities(partials.Events, folded)
	fallback := p.ModelHint
	if meta.ModelHintFallback != "" {
		fallback = meta.ModelHintFallback
	}
	return NormalizedRecord{
		Events:      partials.Events,
		SessionHint: sessionID,
		ModelHint:   ResolveModelHint(partials.Events, harness, fallback),
		CwdHint:     cwd,
	}
}

// ResolveRecordTS mirrors resolve_record_ts: the record's own timestamp, else
// the caller's hint, else (codex only) the rollout file-name timestamp.
func ResolveRecordTS(harness, sourceFile, sourceRecordTS, recordTSHint string) string {
	if trimmed := strings.TrimSpace(sourceRecordTS); trimmed != "" {
		return trimmed
	}
	if hint := strings.TrimSpace(recordTSHint); hint != "" {
		return hint
	}
	if harness == "codex" {
		if fileTS := InferRolloutRecordTSFromFile(sourceFile); fileTS != "" {
			return fileTS
		}
	}
	return ""
}

// ResolveCwd mirrors resolve_cwd: the record's cwd wins over the session
// hint; whitespace-only values count as absent.
func ResolveCwd(recordCwd, cwdHint string) string {
	if trimmed := strings.TrimSpace(recordCwd); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(cwdHint)
}

// FoldToolPayloads ports fold_tool_payloads_into_events: each tool row's
// detail is embedded in its owning event's payload_json under moraine_tool_io
// and its identity fields are copied onto the event. The returned map holds,
// per provisional event uid, the pre-fold semantic material the canonical uid
// hashes.
func FoldToolPayloads(p *Partials) map[string]*FoldedIdentity {
	index := make(map[string]int, len(p.Events))
	for i, ev := range p.Events {
		index[ev.Event.EventUID] = i
	}
	identities := make(map[string]*FoldedIdentity, len(p.Tools))
	for _, tool := range p.Tools {
		i, ok := index[tool.EventUID]
		if !ok {
			continue
		}
		ev := p.Events[i]

		var sourcePayload JSON
		if s := ev.Event.PayloadJSON; s != "" {
			if parsed, err := DecodeJSON([]byte(s)); err == nil {
				sourcePayload = parsed
			} else {
				sourcePayload = s
			}
		}
		canonical := map[string]any{}
		if obj, isObj := sourcePayload.(map[string]any); isObj {
			canonical = obj
		} else if sourcePayload != nil {
			canonical = map[string]any{"source_payload": sourcePayload}
		}

		semantic := make(map[string]any, len(canonical))
		for k, v := range canonical {
			semantic[k] = v
		}
		SanitizeSemanticPayload(semantic)
		semanticPayload := Compact(semantic)

		toolPayload := toolPayloadObject(tool)
		identities[tool.EventUID] = &FoldedIdentity{
			SemanticPayload: semanticPayload,
			ToolFields:      ToolIdentityFields(tool),
		}
		ev.Event.ToolCallID = tool.ToolCallID
		ev.Event.ParentToolCallID = tool.ParentToolCallID
		ev.Event.ToolName = tool.ToolName
		ev.Event.ToolPhase = tool.ToolPhase
		ev.Event.ToolError = tool.ToolError
		if tool.ProjectID != "" {
			ev.Event.ProjectID = tool.ProjectID
		}
		if tool.RepoRelPath != "" {
			ev.Event.RepoRelPath = tool.RepoRelPath
		}
		if tool.WorktreeRoot != "" {
			ev.Event.WorktreeRoot = tool.WorktreeRoot
		}
		canonical["moraine_tool_io"] = toolPayload
		ev.Event.PayloadJSON = Compact(canonical)
	}
	return identities
}

// toolPayloadObject renders the tool row as the moraine_tool_io member: the
// tool row minus the event/session transport columns.
func toolPayloadObject(t *ToolRow) map[string]any {
	full := t.MapOf()
	payload := make(map[string]any, len(full))
	for key, value := range full {
		switch key {
		case "event_uid", "event_version", "session_id", "source_name", "harness", "record_ts":
			continue
		}
		payload[key] = value
	}
	return payload
}

// occurrenceGroup is the dedupe key for identical semantic events from one
// line (SemanticOccurrenceGroup upstream).
type occurrenceGroup struct {
	sourceName   string
	sourceFile   string
	generation   uint64
	lineNo       uint64
	offset       uint64
	baseEventUID string
}

// StampDuplicateSemanticOccurrences ports stamp_duplicate_semantic_occurrences:
// events from the same line that hash to the same canonical uid get a 1-based
// moraine_semantic_occurrence stamped into their payload so their identities
// diverge.
func StampDuplicateSemanticOccurrences(events []*EventRow, folded map[string]*FoldedIdentity) {
	groups := make([]occurrenceGroup, len(events))
	counts := make(map[occurrenceGroup]uint32, len(events))
	for i, ev := range events {
		base := SemanticEventUID(&ev.Event, folded[ev.Event.EventUID])
		g := occurrenceGroup{
			sourceName:   ev.Event.SourceName,
			sourceFile:   ev.Event.SourceFile,
			generation:   uint64(ev.Event.SourceGeneration),
			lineNo:       ev.Event.SourceLineNo,
			offset:       ev.Event.SourceOffset,
			baseEventUID: base,
		}
		groups[i] = g
		counts[g]++
	}
	next := make(map[occurrenceGroup]uint32, len(counts))
	for i, ev := range events {
		g := groups[i]
		if counts[g] < 2 {
			continue
		}
		next[g]++
		stampSemanticOccurrence(ev, next[g])
		if f := folded[ev.Event.EventUID]; f != nil {
			f.Occurrence = next[g]
		}
	}
}

func stampSemanticOccurrence(ev *EventRow, occurrence uint32) {
	parsed, err := DecodeJSON([]byte(ev.Event.PayloadJSON))
	if err != nil {
		return
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return
	}
	obj[semanticOccurrenceField] = jsonNumberUint(uint64(occurrence))
	ev.Event.PayloadJSON = Compact(obj)
}

// FinalizeIdentities ports finalize_event_identities: once every canonical
// field and folded tool payload is present, replace the provisional uids with
// the semantic ones and rewrite same-record references.
func FinalizeIdentities(events []*EventRow, folded map[string]*FoldedIdentity) {
	uidMap := make(map[string]string, len(events))
	for _, ev := range events {
		old := ev.Event.EventUID
		uidMap[old] = SemanticEventUID(&ev.Event, folded[old])
	}
	for _, ev := range events {
		if uid, ok := uidMap[ev.Event.EventUID]; ok {
			ev.Event.EventUID = uid
		}
		if uid, ok := uidMap[ev.Event.OriginEventID]; ok {
			ev.Event.OriginEventID = uid
		}
		rewritePayloadUID(ev, uidMap)
	}
}

// rewritePayloadUID ports rewrite_payload_uid_field for request_event_uid.
func rewritePayloadUID(ev *EventRow, uidMap map[string]string) {
	parsed, err := DecodeJSON([]byte(ev.Event.PayloadJSON))
	if err != nil {
		return
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return
	}
	old, ok := obj["request_event_uid"].(string)
	if !ok {
		return
	}
	uid, ok := uidMap[old]
	if !ok {
		return
	}
	obj["request_event_uid"] = uid
	ev.Event.PayloadJSON = Compact(obj)
}

// EventVersion is the dedupe stamp upstream writes per row (higher wins). It
// is wall-clock based, so the golden masks it.
func EventVersion() uint64 {
	return uint64(time.Now().UnixMilli())
}
