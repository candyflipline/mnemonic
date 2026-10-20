package query

// The open engine: one ordered pass over a session's trace, ported from
// upstream conversations/src/clickhouse_repo/canonical_open.rs
// (scan_canonical_session, update_turn, session_labels, preview_from_wide,
// event_mode_rank, adjacent_turn_ref). The cache is one immutable snapshot per
// file, so the upstream version paging, FINAL/reconcile and
// snapshot-generation machinery is a no-op here; trace carries the wide
// columns inline, so hydration is folded into the scan.

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// navRow is upstream NavRow + DerivedPosition + HydratedRow folded together.
type navRow struct {
	sessionID         string
	eventUID          string
	eventTime         string
	eventUnixMs       int64
	sourceFile        string
	sourceRef         string
	eventClass        string
	actorRole         string
	payloadType       string
	callID            string
	name              string
	phase             string
	itemID            string
	sourceName        string
	harness           string
	inferenceProvider string
	textContent       string
	payloadJSON       string
	tokenUsageJSON    string
	lakeFile          string
	lakeRow           int64
	tokenUsageBuckets map[string]uint64
	tokenUsageNative  map[string]float64
	endpointKind      string

	eventOrder   uint64
	turnSeq      uint32
	eventOrdinal uint32
}

func (r *navRow) eventType() string {
	return eventTypeFor(r.eventClass, r.payloadType, r.actorRole)
}

func (r *navRow) isUserMessage() bool {
	return r.actorRole == "user" && r.eventClass == "message"
}

func (r *navRow) isTerminal() bool {
	return r.payloadType == "task_complete" || r.payloadType == "turn_aborted"
}

// isMetadataBearing ports sql/033_replay_stable_events.sql: session_meta rows,
// plus omp title payloads.
func (r *navRow) isMetadataBearing() bool {
	if r.eventClass == "session_meta" {
		return true
	}
	if r.sourceName != "omp" {
		return false
	}
	t := payloadString(r.payloadJSON, "type")
	return t == "title" || t == "title_change"
}

// payloadString reads payload_json as an object and returns one string field.
func payloadString(payloadJSON, field string) string {
	if strings.TrimSpace(payloadJSON) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &m); err != nil {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

// eventRef is upstream McpEventRef.
type eventRef struct {
	sessionID  string
	eventUID   string
	eventOrder uint64
	turnSeq    uint32
	eventTime  string
	eventType  string
}

func navEventRef(r *navRow) *eventRef {
	return &eventRef{
		sessionID:  r.sessionID,
		eventUID:   r.eventUID,
		eventOrder: r.eventOrder,
		turnSeq:    r.turnSeq,
		eventTime:  r.eventTime,
		eventType:  r.eventType(),
	}
}

// refBefore ports the (event_time, event_order, event_uid) tuple comparison
// scan_canonical_session uses for first/last.
func refBefore(a, b *eventRef) bool {
	if a.eventTime != b.eventTime {
		return a.eventTime < b.eventTime
	}
	if a.eventOrder != b.eventOrder {
		return a.eventOrder < b.eventOrder
	}
	return a.eventUID < b.eventUID
}

// turnRef is upstream McpTurnRef.
type turnRef struct {
	sessionID string
	turnSeq   uint32
	turnID    string
	startedAt string
	endedAt   string
}

func turnRefOf(m *turnSummary) *turnRef {
	return &turnRef{
		sessionID: m.sessionID,
		turnSeq:   m.turnSeq,
		turnID:    m.turnID,
		startedAt: m.startedAt,
		endedAt:   m.endedAt,
	}
}

// sessionMetadata is upstream SessionMetadata.
type sessionMetadata struct {
	sessionID         string
	firstEventTime    string
	firstEventUnixMs  int64
	lastEventTime     string
	lastEventUnixMs   int64
	totalTurns        uint32
	totalEvents       uint64
	userMessages      uint64
	assistantMessages uint64
	toolCalls         uint64
	toolResults       uint64
	mode              string
	firstEventUID     string
	lastEventUID      string
	lastActorRole     string
}

// turnSummary is upstream TurnSummary, the per-turn metadata accumulator.
type turnSummary struct {
	sessionID         string
	turnSeq           uint32
	turnID            string
	startedAt         string
	startedAtUnixMs   int64
	endedAt           string
	endedAtUnixMs     int64
	totalEvents       uint64
	userMessages      uint64
	assistantMessages uint64
	toolCalls         uint64
	toolResults       uint64
	reasoningItems    uint64
}

// turnAccum is upstream TurnAccum (minus cursor-carry plumbing).
type turnAccum struct {
	metadata             *turnSummary
	firstEvent           *eventRef
	lastEvent            *eventRef
	userInput            *navRow
	finalResponse        *navRow
	toolsCalled          []string
	toolNames            map[string]bool
	normalizedEventTypes []string
	seenTypes            map[string]bool
	completed            bool
	terminalEventUID     string
	selected             []*navRow
}

// sessionScan is upstream SessionScan.
type sessionScan struct {
	metadata          sessionMetadata
	source            string
	harness           string
	inferenceProvider string
	ompDispatchTitle  string
	turns             map[uint32]*turnAccum
	turnSeqs          []uint32 // ascending; mirrors the upstream BTreeMap order
	metadataRows      []*navRow
	completed         bool
	terminalEventUID  string
	target            *navRow
	previousEvent     *eventRef
	nextEvent         *eventRef
}

// loadSessionScan ports scan_canonical_session: rows in event_order, per-turn
// accumulators, session signals. selectedTurn (with hasSelectedTurn) marks the
// turn whose events are kept; targetUID ("" = none) marks the open target.
func loadSessionScan(ctx context.Context, db *sql.DB, sessionID string, selectedTurn uint32, hasSelectedTurn bool, targetUID string) (*sessionScan, error) {
	const scanSQL = `SELECT t.session_id, t.event_uid, epoch_ms(t.event_time),
       t.source_file, t.source_ref, t.event_class, t.actor_role, t.payload_type,
       t.call_id, t.name, t.phase, t.item_id,
       t.text_content, e.meta_json, e.lake_file, e.lake_row, t.endpoint_kind,
       cast(to_json(t.token_usage_buckets) AS VARCHAR),
       cast(to_json(t.token_usage_native_units) AS VARCHAR),
       e.source_name, e.harness, e.inference_provider,
       t.event_order, t.turn_seq
FROM trace t LEFT JOIN events e ON e.event_uid = t.event_uid AND e.session_id = %SID%
WHERE t.session_id = %SID%
ORDER BY t.event_order`
	q := strings.ReplaceAll(scanSQL, "%SID%", sqlQuote(sessionID))
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sc := &sessionScan{turns: map[uint32]*turnAccum{}}
	var eventOrder uint64
	var userCount uint32
	ordinals := map[uint32]uint32{}
	var first, last, lastRowRef *eventRef
	var lastActorRole string
	var modeRank uint8
	var targetSeen bool
	var userMsgs, asstMsgs, toolCalls, toolResults uint64
	var ompTime string

	for rows.Next() {
		var r navRow
		var bucketsJSON, nativeJSON sql.NullString
		var srcName, harness, infProv, metaJSON sql.NullString
		if err := rows.Scan(&r.sessionID, &r.eventUID, &r.eventUnixMs,
			&r.sourceFile, &r.sourceRef, &r.eventClass, &r.actorRole, &r.payloadType,
			&r.callID, &r.name, &r.phase, &r.itemID,
			&r.textContent, &metaJSON, &r.lakeFile, &r.lakeRow, &r.endpointKind,
			&bucketsJSON, &nativeJSON,
			&srcName, &harness, &infProv,
			&r.eventOrder, &r.turnSeq); err != nil {
			return nil, err
		}
		r.eventTime = formatRFC3339UTCMillis(r.eventUnixMs)
		r.payloadJSON = metaJSON.String
		r.sourceName = srcName.String
		r.harness = harness.String
		r.inferenceProvider = infProv.String
		r.tokenUsageBuckets = decodeUintMap(bucketsJSON.String)
		r.tokenUsageNative = decodeFloatMap(nativeJSON.String)

		eventOrder++
		if r.isUserMessage() {
			userCount++
		}
		r.eventOrder = eventOrder
		ordinals[r.turnSeq]++
		r.eventOrdinal = ordinals[r.turnSeq]
		ref := navEventRef(&r)

		if first == nil || refBefore(ref, first) {
			first = ref
		}
		if last == nil || refBefore(last, ref) {
			last = ref
			lastActorRole = r.actorRole
		}
		if targetUID != "" && r.eventUID == targetUID {
			row := r
			sc.target = &row
			sc.previousEvent = lastRowRef
			targetSeen = true
		} else if targetSeen && sc.nextEvent == nil {
			sc.nextEvent = ref
		}
		lastRowRef = ref

		if r.isUserMessage() {
			userMsgs++
		} else if r.actorRole == "assistant" && r.eventClass == "message" {
			asstMsgs++
		}
		if r.eventClass == "tool_call" {
			toolCalls++
		}
		if r.eventClass == "tool_result" {
			toolResults++
		}
		if rank := eventModeRank(&r); rank > modeRank {
			modeRank = rank
		}
		if r.sourceName != "" {
			sc.source = r.sourceName
		}
		if r.harness != "" {
			sc.harness = r.harness
		}
		if r.inferenceProvider != "" {
			sc.inferenceProvider = r.inferenceProvider
		}
		if r.sourceName == "omp" && strings.HasSuffix(r.sourceFile, ".jsonl") &&
			!strings.HasSuffix(r.sourceFile, sessionID+".jsonl") {
			base := strings.ReplaceAll(r.sourceFile, "\\", "/")
			if i := strings.LastIndexByte(base, '/'); i >= 0 {
				base = base[i+1:]
			}
			title := strings.TrimSuffix(base, ".jsonl")
			if title != "" && (ompTime == "" || r.eventTime < ompTime) {
				ompTime = r.eventTime
				sc.ompDispatchTitle = title
			}
		}
		if r.isMetadataBearing() {
			row := r
			sc.metadataRows = append(sc.metadataRows, &row)
		}
		if r.isTerminal() {
			sc.completed = r.payloadType == "task_complete"
			sc.terminalEventUID = r.eventUID
		}

		turn := sc.turns[r.turnSeq]
		if turn == nil {
			turn = &turnAccum{toolNames: map[string]bool{}, seenTypes: map[string]bool{}}
			sc.turns[r.turnSeq] = turn
			sc.turnSeqs = append(sc.turnSeqs, r.turnSeq)
		}
		updateTurn(turn, &r, ref, hasSelectedTurn && r.turnSeq == selectedTurn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if first == nil || last == nil {
		return nil, nil
	}
	if err := hydrateKept(ctx, db, sc); err != nil {
		return nil, err
	}
	sort.Slice(sc.turnSeqs, func(i, j int) bool { return sc.turnSeqs[i] < sc.turnSeqs[j] })

	// Session time bounds come from the turn accumulators, as upstream.
	var minStart, maxEnd int64
	for i, seq := range sc.turnSeqs {
		m := sc.turns[seq].metadata
		if i == 0 || m.startedAtUnixMs < minStart {
			minStart = m.startedAtUnixMs
		}
		if i == 0 || m.endedAtUnixMs > maxEnd {
			maxEnd = m.endedAtUnixMs
		}
	}
	var maxTurn uint32
	for _, seq := range sc.turnSeqs {
		if seq > maxTurn {
			maxTurn = seq
		}
	}
	sc.metadata = sessionMetadata{
		sessionID:         sessionID,
		firstEventTime:    first.eventTime,
		firstEventUnixMs:  minStart,
		lastEventTime:     last.eventTime,
		lastEventUnixMs:   maxEnd,
		totalTurns:        maxTurn,
		totalEvents:       eventOrder,
		userMessages:      userMsgs,
		assistantMessages: asstMsgs,
		toolCalls:         toolCalls,
		toolResults:       toolResults,
		mode:              modeFromRank(modeRank),
		firstEventUID:     first.eventUID,
		lastEventUID:      last.eventUID,
		lastActorRole:     lastActorRole,
	}
	return sc, nil
}

func decodeUintMap(s string) map[string]uint64 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var m map[string]uint64
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

func decodeFloatMap(s string) map[string]float64 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// updateTurn ports update_turn.
func updateTurn(turn *turnAccum, r *navRow, ref *eventRef, selected bool) {
	if turn.metadata == nil {
		turn.metadata = &turnSummary{
			sessionID:       r.sessionID,
			turnSeq:         r.turnSeq,
			turnID:          strconv.FormatUint(uint64(r.turnSeq), 10),
			startedAt:       r.eventTime,
			startedAtUnixMs: r.eventUnixMs,
			endedAt:         r.eventTime,
			endedAtUnixMs:   r.eventUnixMs,
		}
	}
	m := turn.metadata
	if r.eventUnixMs < m.startedAtUnixMs {
		m.startedAt = r.eventTime
		m.startedAtUnixMs = r.eventUnixMs
	}
	if r.eventUnixMs > m.endedAtUnixMs {
		m.endedAt = r.eventTime
		m.endedAtUnixMs = r.eventUnixMs
	}
	m.totalEvents++
	if r.isUserMessage() {
		m.userMessages++
	}
	if r.actorRole == "assistant" && r.eventClass == "message" {
		m.assistantMessages++
	}
	if r.eventClass == "tool_call" {
		m.toolCalls++
	}
	if r.eventClass == "tool_result" {
		m.toolResults++
	}
	if r.eventClass == "reasoning" {
		m.reasoningItems++
	}
	if turn.firstEvent == nil {
		turn.firstEvent = ref
	}
	turn.lastEvent = ref
	if isMcpMessageEvent(r.eventClass, r.payloadType) &&
		strings.EqualFold(r.actorRole, "user") && turn.userInput == nil {
		row := *r
		turn.userInput = &row
	}
	if isMcpMessageEvent(r.eventClass, r.payloadType) &&
		strings.EqualFold(r.actorRole, "assistant") &&
		!strings.EqualFold(r.phase, "commentary") {
		row := *r
		turn.finalResponse = &row
	}
	if r.eventClass == "tool_call" && r.name != "" && !turn.toolNames[r.name] {
		turn.toolNames[r.name] = true
		turn.toolsCalled = append(turn.toolsCalled, r.name)
	}
	et := r.eventType()
	if !turn.seenTypes[et] {
		turn.seenTypes[et] = true
		turn.normalizedEventTypes = append(turn.normalizedEventTypes, et)
	}
	if r.isTerminal() {
		turn.completed = r.payloadType == "task_complete"
		turn.terminalEventUID = r.eventUID
	}
	if selected {
		row := *r
		turn.selected = append(turn.selected, &row)
	}
}

// adjacentTurnRef ports adjacent_turn_ref over the ordered turn keys.
func adjacentTurnRef(sc *sessionScan, turnSeq uint32, next bool) *turnRef {
	var seq uint32
	found := false
	if next {
		for _, s := range sc.turnSeqs {
			if s > turnSeq {
				seq, found = s, true
				break
			}
		}
	} else {
		for _, s := range sc.turnSeqs {
			if s < turnSeq {
				seq, found = s, true
			}
		}
	}
	if !found {
		return nil
	}
	turn := sc.turns[seq]
	if turn == nil || turn.metadata == nil {
		return nil
	}
	return turnRefOf(turn.metadata)
}

// turnCompact is upstream McpTurnCompact.
type turnCompact struct {
	metadata             *turnSummary
	userInputSummary     any
	finalResponseSummary any
	userInputEvent       *eventRef
	finalResponseEvent   *eventRef
	toolsCalled          []string
	normalizedEventTypes []string
	completed            bool
	terminalEventUID     string
	firstEvent           *eventRef
	lastEvent            *eventRef
}

// compactTurn ports compact_turn.
func compactTurn(turn *turnAccum, previewChars int) *turnCompact {
	c := &turnCompact{
		metadata:             turn.metadata,
		toolsCalled:          turn.toolsCalled,
		normalizedEventTypes: turn.normalizedEventTypes,
		completed:            turn.completed,
		terminalEventUID:     turn.terminalEventUID,
		firstEvent:           turn.firstEvent,
		lastEvent:            turn.lastEvent,
	}
	if turn.userInput != nil {
		c.userInputSummary = previewFromWide(turn.userInput, previewChars)
		c.userInputEvent = navEventRef(turn.userInput)
	}
	if turn.finalResponse != nil {
		c.finalResponseSummary = previewFromWide(turn.finalResponse, previewChars)
		c.finalResponseEvent = navEventRef(turn.finalResponse)
	}
	return c
}

// sessionLabels ports session_labels; returns (title, slug, summary), each
// nil when empty.
func sessionLabels(sc *sessionScan) (any, any, any) {
	ordered := append([]*navRow(nil), sc.metadataRows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.eventTime != b.eventTime {
			return a.eventTime < b.eventTime
		}
		return a.eventUID < b.eventUID
	})
	title, name, summary, slug := "", "", "", ""
	for _, row := range ordered {
		if v := payloadString(row.payloadJSON, "title"); v != "" {
			title = v
		}
		if row.eventClass == "session_meta" {
			if v := payloadString(row.payloadJSON, "name"); v != "" {
				name = v
			}
			if v := payloadString(row.payloadJSON, "summary"); v != "" {
				summary = v
			}
			if v := payloadString(row.payloadJSON, "slug"); v != "" {
				slug = v
			}
		}
	}
	var resolvedTitle, resolvedSummary string
	if sc.source == "omp" {
		resolvedTitle = firstNonEmpty(title, name, summary, sc.ompDispatchTitle)
		resolvedSummary = firstNonEmpty(summary, resolvedTitle, name, sc.ompDispatchTitle)
	} else {
		resolvedTitle = firstNonEmpty(title, name)
		resolvedSummary = firstNonEmpty(summary, resolvedTitle, name)
	}
	return nonEmpty(resolvedTitle), nonEmpty(slug), nonEmpty(resolvedSummary)
}

// previewFromWide ports preview_from_wide.
func previewFromWide(row *navRow, previewChars int) any {
	source, isPayload := row.textContent, false
	if strings.TrimSpace(row.textContent) == "" {
		source, isPayload = row.payloadJSON, true
	}
	if s, ok := compactSource(source, isPayload, previewChars); ok {
		return s
	}
	return nil
}

// compactSource ports compact_source: returns ("", false) for None.
func compactSource(source string, isPayload bool, previewChars int) (string, bool) {
	if strings.TrimSpace(source) == "" {
		return "", false
	}
	outputLimit := previewChars
	if outputLimit < 1 {
		outputLimit = 1
	}
	sourceLimit := outputLimit
	if sourceLimit < 4 {
		sourceLimit = 4
	}
	if isPayload {
		sourceLimit *= 2
	}
	truncated := source
	if len([]rune(source)) > sourceLimit {
		truncated = string([]rune(source)[:sourceLimit-3]) + "..."
	}
	compact := compactTextLine(truncated, outputLimit)
	if compact == "" {
		return "", false
	}
	return compact, true
}

// eventModeRank ports event_mode_rank.
func eventModeRank(r *navRow) uint8 {
	if r.payloadType == "web_search_call" || r.payloadType == "search_results_received" ||
		(r.payloadType == "tool_use" && (r.name == "WebSearch" || r.name == "WebFetch")) {
		return 3
	}
	if r.sourceName == "codex-mcp" || isMcpInternalToolName(r.name) {
		return 2
	}
	if r.eventClass == "tool_call" || r.eventClass == "tool_result" || r.payloadType == "tool_use" {
		return 1
	}
	return 0
}

// modeFromRank ports mode_from_rank -> ConversationMode::as_str.
func modeFromRank(rank uint8) string {
	switch rank {
	case 3:
		return "web_search"
	case 2:
		return "mcp_internal"
	case 1:
		return "tool_calling"
	default:
		return "chat"
	}
}

// isMcpInternalToolName ports the upstream clickhouse/src/mcp_tool_names.rs
// is_mcp_internal_tool_name (the Go twin of the SQL predicate in common.go).
func isMcpInternalToolName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	const names = "search\x00search_sessions\x00open\x00list_sessions\x00file_attention"
	if strings.Contains("\x00"+names+"\x00", "\x00"+n+"\x00") {
		return true
	}
	parts := strings.Split(n, "__")
	if len(parts) != 3 || parts[0] != "mcp" || (parts[1] != "moraine" && parts[1] != "mnemonic") {
		return false
	}
	return strings.Contains("\x00"+names+"\x00", "\x00"+parts[2]+"\x00")
}
