package main

import "github.com/candyflipline/mnemonic/internal/schema"

// Columns contains only values required by the fixed tool surface.
// The parser schema is unchanged; full payload and usage maps live in heavy blocks.
type Columns struct {
	EventUID          string
	SessionID         string
	SourceName        string
	Harness           string
	InferenceProvider string
	SourceFile        string
	SourceGeneration  uint32
	SourceLineNo      uint64
	SourceOffset      uint64
	EventKind         string
	ActorKind         string
	PayloadType       string
	OpStatus          string
	TurnIndex         uint32
	ItemID            string
	ToolCallID        string
	ToolName          string
	ToolPhase         string
	EndpointKind      string
	TextPreview       string
	PayloadJSON       string
	ProjectID         string
	RepoRelPath       string
	WorktreeRoot      string
	EventVersion      uint64
	Cwd               string
}

func packEvent(e *schema.Event) *event {
	return &event{Columns: Columns{
		EventUID:          e.EventUID,
		SessionID:         e.SessionID,
		SourceName:        e.SourceName,
		Harness:           e.Harness,
		InferenceProvider: e.InferenceProvider,
		SourceFile:        e.SourceFile,
		SourceGeneration:  e.SourceGeneration,
		SourceLineNo:      e.SourceLineNo,
		SourceOffset:      e.SourceOffset,
		EventKind:         e.EventKind,
		ActorKind:         e.ActorKind,
		PayloadType:       e.PayloadType,
		OpStatus:          e.OpStatus,
		TurnIndex:         e.TurnIndex,
		ItemID:            e.ItemID,
		ToolCallID:        e.ToolCallID,
		ToolName:          e.ToolName,
		ToolPhase:         e.ToolPhase,
		EndpointKind:      e.EndpointKind,
		TextPreview:       e.TextPreview,
		ProjectID:         e.ProjectID,
		RepoRelPath:       e.RepoRelPath,
		WorktreeRoot:      e.WorktreeRoot,
		EventVersion:      e.EventVersion,
		Cwd:               e.Cwd,
	}, Time: parseTime(e.RecordTS, fallbackEventMillis), Timestamp: parseTime(e.EventTS, 0)}
}
