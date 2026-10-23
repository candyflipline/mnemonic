package index

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

func init() { register(openTool{}) }

type openTool struct{}

func (openTool) Name() string { return "open" }

type canonicalOpenArgs struct {
	target mcpID
	page   *openPage
	cursor string
	opened SessionFilter
}

type openPage struct {
	limit int
	after *openCursorAfter
}

const (
	openCursorMaxChars = 16384
	openCursorVersion  = 2
)

type openCursor struct {
	Version  int             `json:"version"`
	TargetID string          `json:"target_id"`
	Limit    int             `json:"limit"`
	After    openCursorAfter `json:"after"`
}

type openCursorAfter struct {
	Kind         string `json:"kind"`
	TurnSeq      uint32 `json:"turn_seq,omitempty"`
	EventOrdinal uint32 `json:"event_ordinal,omitempty"`
}

// parseOpenArgs: every message is upstream text.
func parseOpenArgs(raw json.RawMessage) (canonicalOpenArgs, error) {
	var args struct {
		ID     *string `json:"id"`
		Limit  *int64  `json:"limit"`
		Cursor *string `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return canonicalOpenArgs{}, argErrorf("open expects id, optional limit, or a continuation cursor")
	}
	if args.Cursor != nil {
		if args.ID != nil || args.Limit != nil {
			return canonicalOpenArgs{}, argErrorf("cursor must be provided without id or limit")
		}
		cursor := strings.TrimSpace(*args.Cursor)
		if cursor == "" {
			return canonicalOpenArgs{}, argErrorf("cursor must be a non-empty string")
		}
		if len(cursor) > openCursorMaxChars {
			return canonicalOpenArgs{}, argErrorf("cursor must be at most %d characters", openCursorMaxChars)
		}
		decoded, err := decodeOpenCursor(cursor)
		if err != nil {
			return canonicalOpenArgs{}, err
		}
		if decoded.Limit < 1 || decoded.Limit > openMaxResults {
			return canonicalOpenArgs{}, argErrorf("cursor page size is invalid; reopen the target")
		}
		target, err := parseMcpID(decoded.TargetID)
		if err != nil {
			return canonicalOpenArgs{}, argErrorf("cursor target is invalid; reopen the target")
		}
		if target.kind == kindEvent {
			return canonicalOpenArgs{}, argErrorf("cursor target kind is invalid; reopen the target")
		}
		return canonicalOpenArgs{target: target, page: &openPage{limit: decoded.Limit, after: &decoded.After}, cursor: cursor}, nil
	}

	if args.ID == nil {
		return canonicalOpenArgs{}, argErrorf("id is required")
	}
	if strings.TrimSpace(*args.ID) == "" {
		return canonicalOpenArgs{}, argErrorf("id must be a non-empty string")
	}
	id, err := parseMcpID(*args.ID)
	if err != nil {
		return canonicalOpenArgs{}, err
	}
	a := canonicalOpenArgs{target: id}
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > openMaxResults {
			return canonicalOpenArgs{}, argErrorf("limit must be between 1 and %d", openMaxResults)
		}
		if id.kind == kindEvent {
			return canonicalOpenArgs{}, argErrorf("limit is only supported for session and turn IDs")
		}
		a.page = &openPage{limit: int(*args.Limit)}
	}
	return a, nil
}

func decodeOpenCursor(token string) (openCursor, error) {
	bytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return openCursor{}, argErrorf("cursor is malformed; reopen the target")
	}
	var cursor openCursor
	if err := json.Unmarshal(bytes, &cursor); err != nil {
		return openCursor{}, argErrorf("cursor is malformed; reopen the target")
	}
	if cursor.Version != openCursorVersion {
		return openCursor{}, argErrorf("cursor snapshot is stale; reopen the target to restart expansion")
	}
	return cursor, nil
}

func encodeOpenCursor(cursor openCursor) (string, error) {
	bytes, err := json.Marshal(cursor)
	if err != nil {
		return "", argErrorf("failed to encode cursor: %s", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func (a canonicalOpenArgs) requestShape() map[string]any {
	if a.cursor != "" {
		return map[string]any{"cursor": a.cursor}
	}
	if a.page != nil {
		return map[string]any{"id": a.target.String(), "limit": a.page.limit}
	}
	return map[string]any{"id": a.target.String()}
}

func (openTool) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
	args, err := parseOpenArgs(raw)
	if err != nil {
		return nil, "", err
	}
	if args.opened, err = Sessions(ctx, db); err != nil {
		return nil, "", err
	}
	request := args.requestShape()

	switch args.target.kind {
	case kindSession:
		return openSessionFlow(db, request, args)
	case kindTurn:
		return openTurnFlow(db, request, args)
	default:
		return openEventFlow(db, request, args)
	}
}

func openSessionFlow(db *DB, request map[string]any, args canonicalOpenArgs) (any, string, error) {
	scan, err := db.loadSessionScan(args.target.session, scanOpts{opened: args.opened})
	if err != nil {
		return nil, "", err
	}
	if scan == nil {
		return nil, "", argErrorf("session not found")
	}
	page := args.page
	if page == nil {
		data := openSessionData(scan, nil, nil)
		return data, openResultText(request, data), nil
	}
	limit := page.limit
	afterSeq := uint32(0)
	if page.after != nil {
		afterSeq = page.after.TurnSeq
	}
	var seqs []uint32
	for _, seq := range scan.turnSeqs() {
		if seq > afterSeq {
			seqs = append(seqs, seq)
		}
	}
	if page.after != nil && len(seqs) == 0 {
		// Upstream reports an exhausted continuation as a stale snapshot.
		return nil, "", argErrorf("cursor snapshot is stale; reopen the target to restart expansion")
	}
	hasMore := len(seqs) > limit
	if hasMore {
		seqs = seqs[:limit]
	}
	turns := make([]any, 0, len(seqs))
	for _, seq := range seqs {
		turns = append(turns, openSessionTurnSummary(compactTurn(scan.turns[seq], summaryPreviewChars)))
	}
	var nextCursor any
	if hasMore {
		encoded, err := encodeOpenCursor(openCursor{
			Version:  openCursorVersion,
			TargetID: args.target.String(),
			Limit:    limit,
			After:    openCursorAfter{Kind: "turn", TurnSeq: seqs[len(seqs)-1]},
		})
		if err != nil {
			return nil, "", err
		}
		nextCursor = encoded
	}
	data := openSessionData(scan, turns, nextCursor)
	return data, openResultText(request, data), nil
}

func openTurnFlow(db *DB, request map[string]any, args canonicalOpenArgs) (any, string, error) {
	scan, err := db.loadSessionScan(args.target.session, scanOpts{turn: &args.target.turnSeq, opened: args.opened})
	if err != nil {
		return nil, "", err
	}
	if scan == nil {
		return nil, "", argErrorf("turn not found")
	}
	turn := scan.turns[args.target.turnSeq]
	if turn == nil {
		return nil, "", argErrorf("turn not found")
	}
	compact := compactTurn(turn, summaryPreviewChars)

	afterOrdinal := uint32(0)
	limit := 0
	var pageRows []*navRow
	if page := args.page; page != nil {
		if page.after != nil {
			if page.after.Kind != "event" {
				return nil, "", argErrorf("cursor is malformed; reopen the target")
			}
			afterOrdinal = page.after.EventOrdinal
		}
		limit = page.limit
		for _, row := range turn.selected {
			if row.eventOrdinal > afterOrdinal {
				pageRows = append(pageRows, row)
			}
		}
	}
	hasMore := len(pageRows) > limit
	if hasMore {
		pageRows = pageRows[:limit]
	}
	events := make([]any, 0, len(pageRows))
	for i, row := range pageRows {
		events = append(events, openTurnEventSummary(row, int(afterOrdinal)+i+1, turn.terminalEventUID))
	}
	var nextCursor any
	if hasMore {
		encoded, err := encodeOpenCursor(openCursor{
			Version:  openCursorVersion,
			TargetID: args.target.String(),
			Limit:    limit,
			After:    openCursorAfter{Kind: "event", EventOrdinal: pageRows[len(pageRows)-1].eventOrdinal},
		})
		if err != nil {
			return nil, "", err
		}
		nextCursor = encoded
	}
	data := openTurnData(scan, turn, compact, events, nextCursor)
	return data, openResultText(request, data), nil
}

func openEventFlow(db *DB, request map[string]any, args canonicalOpenArgs) (any, string, error) {
	uid := args.target.eventUID
	sessionID, ok, err := db.sessionOfUID(uid)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", argErrorf("event not found")
	}
	scan, err := db.loadSessionScan(sessionID, scanOpts{targetUID: uid, opened: args.opened})
	if err != nil {
		return nil, "", err
	}
	if scan == nil || scan.target == nil {
		return nil, "", argErrorf("event not found")
	}
	parentTurn := scan.turns[scan.target.turnSeq]
	if parentTurn == nil {
		return nil, "", argErrorf("event not found")
	}
	data := openEventData(scan, scan.target, parentTurn)
	return data, openResultText(request, data), nil
}

func optionalEventID(uid string) any {
	if uid == "" {
		return nil
	}
	return formatEventID(uid)
}

func optionalEventRefID(ref *eventRef) any {
	if ref == nil {
		return nil
	}
	return formatEventID(ref.eventUID)
}

func optionalTurnRefID(ref *turnRef) any {
	if ref == nil {
		return nil
	}
	return formatTurnID(ref.sessionID, ref.turnSeq)
}

func openSessionData(scan *sessionScan, pageTurns []any, nextCursor any) map[string]any {
	m := scan.metadata
	seqs := scan.turnSeqs()
	label := sessionLabels(scan)
	if pageTurns == nil {
		pageTurns = []any{}
	}
	return map[string]any{
		"kind": "session",
		"session": map[string]any{
			"id":                 formatSessionID(m.sessionID),
			"title":              compactOptionalLine(label.title, summaryPreviewChars),
			"source":             compactOptionalLine(scan.source, summaryToolNameChars),
			"started_at":         formatRFC3339UTCMillis(m.firstEventUnixMs),
			"updated_at":         formatRFC3339UTCMillis(m.lastEventUnixMs),
			"completed":          scan.completed,
			"terminal_event_id":  optionalEventID(scan.terminalEventUID),
			"turn_count":         m.totalTurns,
			"event_count":        m.totalEvents,
			"mode":               m.mode,
			"harness":            compactOptionalLine(scan.harness, summaryToolNameChars),
			"inference_provider": compactOptionalLine(scan.inferenceProvider, summaryToolNameChars),
			"session_slug":       compactOptionalLine(label.slug, summaryPreviewChars),
			"session_summary":    compactOptionalLine(label.summary, summaryPreviewChars),
		},
		"turns":       pageTurns,
		"next_cursor": nextCursor,
		"traversal": map[string]any{
			"previous_session_id": nil,
			"next_session_id":     nil,
			"first_turn_id":       formatTurnID(m.sessionID, seqs[0]),
			"last_turn_id":        formatTurnID(m.sessionID, seqs[len(seqs)-1]),
		},
	}
}

func openSessionTurnSummary(c *turnCompact) map[string]any {
	turnID := formatTurnID(c.metadata.sessionID, c.metadata.turnSeq)
	terminalEventID := optionalEventID(c.terminalEventUID)
	userInputEventID := optionalEventRefID(c.userInputEvent)
	finalResponseEventID := optionalEventRefID(c.finalResponseEvent)
	toolsCalled, toolsTruncated := compactTools(c.toolsCalled)
	return map[string]any{
		"id":                     turnID,
		"ordinal":                c.metadata.turnSeq,
		"completed":              c.completed,
		"terminal_event_id":      terminalEventID,
		"event_count":            c.metadata.totalEvents,
		"started_at":             formatRFC3339UTCMillis(c.metadata.startedAtUnixMs),
		"updated_at":             formatRFC3339UTCMillis(c.metadata.endedAtUnixMs),
		"user_input":             compactTextContent(userInputEventID, c.userInputSummary),
		"final_response":         compactTextContent(finalResponseEventID, c.finalResponseSummary),
		"tools_called":           toolsCalled,
		"tools_called_truncated": toolsTruncated,
		"event_types":            stringSliceAny(c.normalizedEventTypes),
		"open": map[string]any{
			"turn_id":           turnID,
			"terminal_event_id": terminalEventID,
		},
	}
}

func compactTextContent(eventID any, text string) any {
	if strings.TrimSpace(text) == "" {
		if eventID != nil {
			return map[string]any{"event_id": eventID, "text": nil, "truncated": false}
		}
		return nil
	}
	compact := compactTextLine(text, summaryPreviewChars)
	return map[string]any{
		"event_id":  eventID,
		"text":      compact,
		"truncated": looksTruncated(compact),
	}
}

func compactTools(tools []string) ([]any, bool) {
	out := make([]any, 0, len(tools))
	for i, tool := range tools {
		if i >= summaryMaxTools {
			break
		}
		out = append(out, compactTextLine(tool, summaryToolNameChars))
	}
	return out, len(tools) > summaryMaxTools
}

func stringSliceAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

func openTurnData(scan *sessionScan, turn *turnAccum, c *turnCompact, events []any, nextCursor any) map[string]any {
	m := c.metadata
	turnID := formatTurnID(m.sessionID, m.turnSeq)
	sessionID := formatSessionID(m.sessionID)
	toolsCalled, toolsTruncated := compactTools(c.toolsCalled)
	if events == nil {
		events = []any{}
	}
	return map[string]any{
		"kind": "turn",
		"turn": map[string]any{
			"id":                turnID,
			"session_id":        sessionID,
			"ordinal":           m.turnSeq,
			"completed":         c.completed,
			"terminal_event_id": optionalEventID(c.terminalEventUID),
			"event_count":       m.totalEvents,
			"started_at":        formatRFC3339UTCMillis(m.startedAtUnixMs),
			"updated_at":        formatRFC3339UTCMillis(m.endedAtUnixMs),
		},
		"session": map[string]any{
			"id":     sessionID,
			"title":  nil,
			"source": nonEmpty(scan.source),
		},
		"summary": map[string]any{
			"user_input":             compactTextContent(optionalEventRefID(c.userInputEvent), c.userInputSummary),
			"final_response":         compactTextContent(optionalEventRefID(c.finalResponseEvent), c.finalResponseSummary),
			"tools_called":           toolsCalled,
			"tools_called_truncated": toolsTruncated,
			"event_types":            stringSliceAny(c.normalizedEventTypes),
		},
		"events":      events,
		"next_cursor": nextCursor,
		"traversal": map[string]any{
			"session_id":       sessionID,
			"previous_turn_id": optionalTurnRefID(adjacentTurnRef(scan, m.turnSeq, false)),
			"next_turn_id":     optionalTurnRefID(adjacentTurnRef(scan, m.turnSeq, true)),
			"first_event_id":   optionalEventRefID(c.firstEvent),
			"last_event_id":    optionalEventRefID(c.lastEvent),
		},
	}
}

func openTurnEventSummary(r *navRow, ordinal int, terminalEventUID string) map[string]any {
	var toolName any
	if name := toolNameFor(r.eventType(), r.name, r.callID); name != "" {
		toolName = compactTextLine(name, summaryToolNameChars)
	}
	summary, truncated := compactEventSummary(r)
	return map[string]any{
		"id":        formatEventID(r.eventUID),
		"ordinal":   ordinal,
		"type":      r.eventType(),
		"timestamp": formatRFC3339UTCMillis(r.eventUnixMs),
		"terminal":  terminalEventUID == r.eventUID,
		"tool_name": toolName,
		"model":     nil,
		"summary":   summary,
		"truncated": truncated,
	}
}

func compactEventSummary(r *navRow) (string, bool) {
	text := previewFromWide(r, summaryPreviewChars)
	if text == "" {
		return "", false
	}
	if r.eventType() == "reasoning" && hasEncryptedContentField(text) {
		return encryptedReasoningText, false
	}
	return compactTextLine(text, summaryPreviewChars), looksTruncated(text)
}

func openEventData(scan *sessionScan, r *navRow, parentTurn *turnAccum) map[string]any {
	m := scan.metadata
	eventID := formatEventID(r.eventUID)
	sessionID := formatSessionID(r.sessionID)
	turnID := formatTurnID(r.sessionID, r.turnSeq)
	var terminal bool
	if parentTurn.terminalEventUID != "" {
		terminal = parentTurn.terminalEventUID == r.eventUID
	} else {
		terminal = r.isTerminal()
	}
	payload := parsePayloadJSON(r.payloadJSON)
	toolName := toolNameFor(r.eventType(), r.name, r.callID)
	return map[string]any{
		"kind": "event",
		"event": map[string]any{
			"id":                eventID,
			"session_id":        sessionID,
			"turn_id":           turnID,
			"ordinal":           r.eventOrdinal,
			"type":              r.eventType(),
			"timestamp":         formatRFC3339UTCMillis(r.eventUnixMs),
			"terminal":          terminal,
			"model":             nonEmpty(extractStringField(payload, "model", "model_name")),
			"originating_model": nonEmpty(extractStringField(payload, "originating_model", "model", "model_name")),
			"tool_name":         nonEmpty(toolName),
		},
		"content": fullEventContent(r, r.eventType(), toolName, payload),
		"session": map[string]any{
			"id":          sessionID,
			"title":       nil,
			"source":      nonEmpty(scan.source),
			"started_at":  formatRFC3339UTCMillis(m.firstEventUnixMs),
			"updated_at":  formatRFC3339UTCMillis(m.lastEventUnixMs),
			"turn_count":  m.totalTurns,
			"event_count": m.totalEvents,
			"mode":        m.mode,
		},
		"turn": map[string]any{
			"id":        turnID,
			"ordinal":   parentTurn.metadata.turnSeq,
			"completed": parentTurn.completed,
		},
		"traversal": map[string]any{
			"session_id":        sessionID,
			"turn_id":           turnID,
			"previous_event_id": optionalEventRefID(scan.previousEvent),
			"next_event_id":     optionalEventRefID(scan.nextEvent),
			"previous_turn_id":  optionalTurnRefID(adjacentTurnRef(scan, r.turnSeq, false)),
			"next_turn_id":      optionalTurnRefID(adjacentTurnRef(scan, r.turnSeq, true)),
		},
	}
}

func fullEventContent(r *navRow, eventType string, toolName string, payload any) map[string]any {
	text := fullEventText(r, payload)
	format := "text"
	switch eventType {
	case "tool_call", "tool_response", "reasoning":
		format = eventType
	}
	content := map[string]any{
		"format":    format,
		"text":      text,
		"truncated": false,
	}
	if toolName != "" {
		content["tool_name"] = toolName
	}
	if format == "tool_call" {
		if args := extractToolArguments(payload); args != nil {
			content["arguments"] = args
		} else if payload != nil {
			content["arguments"] = payload
		} else {
			content["arguments"] = nil
		}
	}
	if format == "tool_response" {
		if exitCode, ok := extractExitCode(payload); ok {
			content["exit_code"] = exitCode
		}
	}
	if payload != nil {
		content["payload"] = payload
	} else if strings.TrimSpace(r.payloadJSON) != "" {
		content["payload_json"] = r.payloadJSON
	}
	if strings.TrimSpace(r.tokenUsageJSON) != "" {
		content["token_usage_json"] = r.tokenUsageJSON
	}
	if len(r.tokenUsageBuckets) > 0 {
		content["token_usage_buckets"] = r.tokenUsageBuckets
	}
	if len(r.tokenUsageNative) > 0 {
		content["token_usage_native_units"] = r.tokenUsageNative
	}
	return content
}

func fullEventText(r *navRow, payload any) string {
	if strings.TrimSpace(r.textContent) != "" {
		return r.textContent
	}
	if payload != nil {
		if b, err := json.MarshalIndent(payload, "", "  "); err == nil {
			return string(b)
		}
		b, _ := json.Marshal(payload)
		return string(b)
	}
	return r.payloadJSON
}

// parsePayloadJSON: UseNumber keeps integer literals exact, matching
// serde_json's u64/i64 Value numbers when the payload is re-encoded into the
// content object.
func parsePayloadJSON(payloadJSON string) any {
	if strings.TrimSpace(payloadJSON) == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(payloadJSON))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return nil
	}
	return payload
}

func extractToolArguments(payload any) any {
	object, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"arguments", "args", "input", "parameters"} {
		if value, ok := object[key]; ok {
			return value
		}
	}
	return nil
}

func extractExitCode(payload any) (int64, bool) {
	object, ok := payload.(map[string]any)
	if !ok {
		return 0, false
	}
	for _, key := range []string{"exit_code", "exitCode", "status_code", "code"} {
		// serde's as_i64: only integer numbers convert.
		if n, ok := object[key].(json.Number); ok {
			if v, err := n.Int64(); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func extractStringField(payload any, names ...string) string {
	object, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	for _, name := range names {
		if value, ok := object[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func toolNameFor(eventType, name, callID string) string {
	if eventType != "tool_call" && eventType != "tool_response" {
		return ""
	}
	return firstNonEmpty(name, callID)
}

func pathStr(value any, path ...string) (string, bool) {
	for _, key := range path {
		m, ok := value.(map[string]any)
		if !ok {
			return "", false
		}
		value = m[key]
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func openResultText(request, data map[string]any) string {
	header := openResultHeader(request, data)
	var body []string
	switch kind, _ := pathStr(data, "kind"); kind {
	case "event":
		body = openEventText(data)
	case "turn":
		body = openTurnText(data)
	case "session":
		body = openSessionText(data)
	}
	if len(body) == 0 {
		return header
	}
	return header + "\n\n" + strings.Join(body, "\n")
}

func openErrorText(code, message string) string {
	if code == "" {
		code = "error"
	}
	if message == "" {
		message = "open failed"
	}
	return fmt.Sprintf("open failed (%s): %s", code, message)
}

func openEventText(data map[string]any) []string {
	var lines []string
	eventType := "event"
	if s, ok := pathStr(data, "event", "type"); ok {
		eventType = s
	}
	meta := []string{"type=" + eventType}
	if tool, ok := pathStr(data, "event", "tool_name"); ok {
		meta = append(meta, "tool="+tool)
	}
	if turn, ok := pathStr(data, "turn", "id"); ok {
		meta = append(meta, "turn="+turn)
	}
	if timestamp, ok := pathStr(data, "event", "timestamp"); ok {
		meta = append(meta, "at="+timestamp)
	}
	lines = append(lines, strings.Join(meta, " "))
	if content, ok := data["content"].(map[string]any); ok {
		if arguments, ok := content["arguments"]; ok && arguments != nil {
			encoded, _ := json.Marshal(arguments)
			lines = append(lines, "arguments: "+string(encoded))
		}
		// serde as_i64: only integer numbers render a line.
		if n, ok := content["exit_code"].(int64); ok {
			lines = append(lines, fmt.Sprintf("exit_code: %d", n))
		}
		if text, ok := pathStr(content, "text"); ok {
			lines = append(lines, "", text)
		}
	}
	var neighbors []string
	if id, ok := pathStr(data, "traversal", "previous_event_id"); ok {
		neighbors = append(neighbors, "previous_event="+id)
	}
	if id, ok := pathStr(data, "traversal", "next_event_id"); ok {
		neighbors = append(neighbors, "next_event="+id)
	}
	if len(neighbors) > 0 {
		lines = append(lines, "", strings.Join(neighbors, " "))
	}
	return lines
}

func openTurnText(data map[string]any) []string {
	var lines []string
	if text, ok := pathStr(data, "summary", "user_input", "text"); ok {
		lines = append(lines, "user: "+text)
	}
	if text, ok := pathStr(data, "summary", "final_response", "text"); ok {
		lines = append(lines, "assistant: "+text)
	}
	events, _ := data["events"].([]any)
	if len(lines) > 0 && len(events) > 0 {
		lines = append(lines, "")
	}
	for _, event := range events {
		m, _ := event.(map[string]any)
		ordinal := jsonAsInt64(m["ordinal"])
		eventType := "event"
		if s, ok := pathStr(m, "type"); ok {
			eventType = s
		}
		tool := ""
		if s, ok := pathStr(m, "tool_name"); ok {
			tool = " [" + s + "]"
		}
		summary, _ := pathStr(m, "summary")
		id, _ := pathStr(m, "id")
		lines = append(lines, fmt.Sprintf("%d. %s%s: %s (%s)", ordinal, eventType, tool, summary, id))
	}
	return lines
}

func openSessionText(data map[string]any) []string {
	var lines []string
	if title, ok := pathStr(data, "session", "title"); ok {
		lines = append(lines, "title: "+title)
	}
	if summary, ok := pathStr(data, "session", "session_summary"); ok {
		lines = append(lines, "summary: "+summary)
	}
	turns, _ := data["turns"].([]any)
	if len(lines) > 0 && len(turns) > 0 {
		lines = append(lines, "")
	}
	for _, turn := range turns {
		m, _ := turn.(map[string]any)
		ordinal := jsonAsInt64(m["ordinal"])
		user, _ := pathStr(m, "user_input", "text")
		response, _ := pathStr(m, "final_response", "text")
		id, _ := pathStr(m, "id")
		lines = append(lines, fmt.Sprintf("%d. user: %s | assistant: %s (%s)", ordinal, user, response, id))
	}
	return lines
}

func openResultHeader(request, data map[string]any) string {
	kind := "object"
	if s, ok := pathStr(data, "kind"); ok {
		kind = s
	}
	id := ""
	if s, ok := pathStr(request, "id"); ok {
		id = s
	} else if s, ok := pathStr(data, "turn", "id"); ok {
		id = s
	} else if s, ok := pathStr(data, "event", "id"); ok {
		id = s
	} else if s, ok := pathStr(data, "session", "id"); ok {
		id = s
	}
	if kind == "event" {
		return fmt.Sprintf("Opened event %s.", id)
	}

	var children, total int
	var unit string
	if kind == "session" {
		if turns, ok := data["turns"].([]any); ok {
			children = len(turns)
		}
		if session, ok := data["session"].(map[string]any); ok {
			total = int(jsonAsInt64(session["turn_count"]))
		}
		unit = "turns"
	} else {
		if events, ok := data["events"].([]any); ok {
			children = len(events)
		}
		turnMap, _ := data["turn"].(map[string]any)
		total = int(jsonAsInt64(turnMap["event_count"]))
		unit = "events"
	}
	_, hasLimit := request["limit"]
	_, hasCursor := request["cursor"]
	if !hasLimit && !hasCursor {
		return fmt.Sprintf("Opened %s %s summary only. %d %s available; call open with id and limit to expand.",
			kind, id, total, unit)
	}
	if s, ok := data["next_cursor"].(string); ok && s != "" {
		return fmt.Sprintf("Opened %s %s with %d %s. More are available with next_cursor.",
			kind, id, children, unit)
	}
	return fmt.Sprintf("Opened %s %s with the final %d %s.", kind, id, children, unit)
}
