package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type listSessions struct{}

func (listSessions) Name() string { return "list_sessions" }

func init() { register(listSessions{}) }

const (
	lsDefaultLimit = 20
	lsMinLimit     = 1
	lsMaxResults   = 25
)

type lsArgs struct {
	StartDatetime, EndDatetime string
	StartUnixMs, EndUnixMs     int64
	Limit                      int
	Cursor                     string
	Mode                       string // "" = unset
	Harness, Source            *string
	Sort                       string // "asc" | "desc"
}

var lsModes = map[string]bool{"web_search": true, "mcp_internal": true, "tool_calling": true, "chat": true}

func (listSessions) Call(ctx context.Context, db *sql.DB, args json.RawMessage) (any, string, error) {
	a, err := lsParseArgs(args)
	if err != nil {
		return nil, "", err
	}
	page, err := lsQueryPage(ctx, db, a)
	if err != nil {
		return nil, "", err
	}
	payload := map[string]any{
		"request": lsCanonicalRequest(a),
		"data":    lsDataJSON(a, page),
	}
	return payload["data"], lsText(payload), nil
}

// lsParseArgs: every serde-level failure carries the same upstream message.
func lsParseArgs(raw json.RawMessage) (*lsArgs, error) {
	fields, err := lsObjectFields(raw,
		"start_datetime", "end_datetime", "limit", "cursor", "mode", "harness", "source", "sort")
	if err != nil {
		return nil, lsSerdeError()
	}
	var in struct {
		start, end   *string
		limit        *int64
		cursor       *string
		mode         *string
		harness, src *string
		sort         *string
	}
	str := func(key string) (*string, error) {
		v, ok := fields[key]
		if !ok || string(v) == "null" {
			return nil, nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, err
		}
		return &s, nil
	}
	if in.start, err = str("start_datetime"); err != nil {
		return nil, lsSerdeError()
	}
	if in.end, err = str("end_datetime"); err != nil {
		return nil, lsSerdeError()
	}
	if in.cursor, err = str("cursor"); err != nil {
		return nil, lsSerdeError()
	}
	if in.harness, err = str("harness"); err != nil {
		return nil, lsSerdeError()
	}
	if in.src, err = str("source"); err != nil {
		return nil, lsSerdeError()
	}
	if v, ok := fields["limit"]; ok && string(v) != "null" {
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, lsSerdeError()
		}
		lim, perr := n.Int64()
		if perr != nil {
			return nil, lsSerdeError()
		}
		in.limit = &lim
	}
	if in.mode, err = str("mode"); err != nil {
		return nil, lsSerdeError()
	}
	if in.mode != nil && !lsModes[*in.mode] {
		return nil, lsSerdeError()
	}
	if in.sort, err = str("sort"); err != nil {
		return nil, lsSerdeError()
	}
	if in.sort != nil && *in.sort != "asc" && *in.sort != "desc" {
		return nil, lsSerdeError()
	}

	a := &lsArgs{Sort: "desc"}
	maxLimit := lsMaxResults
	if maxLimit < lsMinLimit {
		maxLimit = lsMinLimit
	}
	a.Limit = lsDefaultLimit
	if a.Limit > maxLimit {
		a.Limit = maxLimit
	}
	if in.limit != nil {
		a.Limit = int(*in.limit)
	}
	if a.Limit < lsMinLimit || a.Limit > maxLimit {
		return nil, qErr("limit must be between 1 and %d", maxLimit)
	}
	if in.start == nil || in.end == nil {
		return nil, qErr("list_sessions requires start_datetime and end_datetime with explicit timezones")
	}
	a.StartDatetime = strings.TrimSpace(*in.start)
	a.EndDatetime = strings.TrimSpace(*in.end)
	var aerr *ArgError
	if a.StartUnixMs, a.StartDatetime, aerr = qParseOptionalDatetime("start_datetime", a.StartDatetime); aerr != nil {
		return nil, aerr
	}
	if a.EndUnixMs, a.EndDatetime, aerr = qParseOptionalDatetime("end_datetime", a.EndDatetime); aerr != nil {
		return nil, aerr
	}
	if a.EndUnixMs <= a.StartUnixMs {
		return nil, qErr("end_datetime must be later than start_datetime")
	}
	if in.cursor != nil {
		a.Cursor = strings.TrimSpace(*in.cursor)
		if a.Cursor == "" {
			return nil, qErr("cursor must be a non-empty string when provided")
		}
	}
	if a.Harness, aerr = qNonEmptyFilter("harness", derefStr(in.harness), in.harness != nil); aerr != nil {
		return nil, aerr
	}
	if a.Source, aerr = qNonEmptyFilter("source", derefStr(in.src), in.src != nil); aerr != nil {
		return nil, aerr
	}
	if in.mode != nil {
		a.Mode = *in.mode
	}
	if in.sort != nil {
		a.Sort = *in.sort
	}
	return a, nil
}

func lsSerdeError() error {
	return qErr("list_sessions expects a JSON object with start_datetime and end_datetime")
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// lsObjectFields decodes a JSON object's top-level fields, rejecting
// non-objects and unknown fields the way serde's deny_unknown_fields does.
func lsObjectFields(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, err
	}
	for key := range fields {
		ok := false
		for _, a := range allowed {
			if key == a {
				ok = true
				break
			}
		}
		if !ok {
			return nil, qErr("unknown field %q", key)
		}
	}
	return fields, nil
}

func lsCanonicalRequest(a *lsArgs) map[string]any {
	req := map[string]any{
		"start_datetime": a.StartDatetime,
		"end_datetime":   a.EndDatetime,
		"sort":           a.Sort,
		"limit":          a.Limit,
		"cursor":         nil,
		"harness":        nil,
		"source":         nil,
	}
	if a.Cursor != "" {
		req["cursor"] = a.Cursor
	}
	if a.Mode != "" {
		req["mode"] = a.Mode
	} else {
		req["mode"] = nil
	}
	if a.Harness != nil {
		req["harness"] = *a.Harness
	}
	if a.Source != nil {
		req["source"] = *a.Source
	}
	return req
}

type lsItem struct {
	SessionID                         string
	FirstEventTime, LastEventTime     string
	FirstEventUnixMs, LastEventUnixMs int64
	TotalTurns                        int
	TotalEvents                       int64
	Mode                              string
	Completed                         bool
	Title                             *string
	Source, Harness                   *string
	SessionSlug, SessionSummary       *string
	DisplayLabel                      string
}

type lsPage struct {
	Items      []lsItem
	NextCursor *string
}

func lsFilterSig(a *lsArgs) string {
	b, err := json.Marshal([]any{a.StartUnixMs, a.EndUnixMs, optStr(a.Mode), a.Harness, a.Source, a.Sort, nil})
	if err != nil {
		return ""
	}
	return string(b)
}

func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type lsCursor struct {
	LastEventUnixMs int64
	SessionID       string
	FilterSig       string
	Sort            string
}

func lsEncodeCursor(c *lsCursor) (*string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	token := qB64URL(b)
	return &token, nil
}

func lsDecodeCursor(token string) (*lsCursor, error) {
	raw, err := qDecodeB64URL(token)
	if err != nil {
		return nil, qErr("invalid base64 cursor")
	}
	var c lsCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, qErr("invalid cursor payload")
	}
	return &c, nil
}

func lsQueryPage(ctx context.Context, db *sql.DB, a *lsArgs) (*lsPage, error) {
	var cursor *lsCursor
	if a.Cursor != "" {
		c, err := lsDecodeCursor(a.Cursor)
		if err != nil {
			return nil, err
		}
		if c.FilterSig != lsFilterSig(a) {
			return nil, qErr("cursor does not match current list_sessions filter")
		}
		if c.Sort != a.Sort {
			return nil, qErr("cursor sort does not match requested sort order")
		}
		cursor = c
	}
	items, err := lsSelectPage(ctx, db, a, cursor)
	if err != nil {
		return nil, err
	}
	hasMore := len(items) > a.Limit
	if hasMore {
		items = items[:a.Limit]
	}
	if len(items) == 0 {
		return &lsPage{}, nil
	}
	var next *string
	if hasMore {
		last := items[len(items)-1]
		token, err := lsEncodeCursor(&lsCursor{
			LastEventUnixMs: last.LastEventUnixMs,
			SessionID:       last.SessionID,
			FilterSig:       lsFilterSig(a),
			Sort:            a.Sort,
		})
		if err != nil {
			return nil, err
		}
		next = token
	}
	return &lsPage{Items: items, NextCursor: next}, nil
}

func lsDataJSON(a *lsArgs, page *lsPage) map[string]any {
	sessions := make([]any, 0, len(page.Items))
	for _, item := range page.Items {
		id, err := qTypedID("session:", item.SessionID)
		if err != nil {
			continue
		}
		sessions = append(sessions, map[string]any{
			"rank": len(sessions) + 1,
			"id":   id,
			"session": map[string]any{
				"id":              id,
				"title":           nilPtr(item.Title),
				"display_label":   item.DisplayLabel,
				"harness":         nilPtr(item.Harness),
				"source":          nilPtr(item.Source),
				"started_at":      qFormatMillis(item.FirstEventUnixMs),
				"updated_at":      qFormatMillis(item.LastEventUnixMs),
				"completed":       item.Completed,
				"turn_count":      item.TotalTurns,
				"event_count":     item.TotalEvents,
				"mode":            item.Mode,
				"session_slug":    nilPtr(item.SessionSlug),
				"session_summary": nilPtr(item.SessionSummary),
			},
			"open": map[string]any{"session_id": id},
		})
	}
	return map[string]any{
		"result_count": len(sessions),
		"limit":        a.Limit,
		"truncated":    page.NextCursor != nil,
		"sessions":     sessions,
		"next_cursor":  nilPtr(page.NextCursor),
	}
}

// nilPtr erases a typed nil pointer to untyped nil, matching how the
// upstream Option serializes; it also lets text checks type-assert strings.
func nilPtr[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func lsText(payload map[string]any) string {
	req, _ := payload["request"].(map[string]any)
	start := stringAt(req, "start_datetime")
	end := stringAt(req, "end_datetime")
	data, _ := payload["data"].(map[string]any)
	count := intAt(data, "result_count")
	lines := []string{sprintf("Found %d session(s) overlapping %s to %s.", count, start, end)}
	if sessions, ok := data["sessions"].([]any); ok {
		for i, s := range sessions {
			if i >= 10 {
				break
			}
			session, _ := s.(map[string]any)
			rank := intAt(session, "rank")
			open, _ := session["open"].(map[string]any)
			sid := stringAt(open, "session_id")
			label := stringOr(session, "session", "display_label", "session")
			lines = append(lines, sprintf("%d. %s (%s)", rank, label, sid))
		}
	}
	if _, ok := data["next_cursor"].(string); ok {
		lines = append(lines, "More sessions are available with next_cursor.")
	}
	return strings.Join(lines, "\n")
}

func stringAt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func intAt(m map[string]any, key string) int {
	n, _ := intAtOK(m, key)
	return n
}

func intAtOK(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	n, ok := m[key].(int)
	return n, ok
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// stringOr reads m[outer][inner] with a fallback. Presence, not emptiness,
// decides: an empty string is still a value (Rust's unwrap_or fires on None).
func stringOr(m map[string]any, outer, inner, fallback string) string {
	if m == nil {
		return fallback
	}
	innerMap, _ := m[outer].(map[string]any)
	if innerMap == nil {
		return fallback
	}
	if v, ok := innerMap[inner]; ok {
		s, _ := v.(string)
		return s
	}
	return fallback
}
