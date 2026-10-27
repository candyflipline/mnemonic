package index

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/ql"
)

func init() { register(queryTool{}) }

const (
	queryDefaultLimit = 100
	queryMaxLimit     = 10000
	queryMaxStatement = 20000
	queryMaxRange     = 31 * 24 * time.Hour
	queryTimeout      = 30 * time.Second
	queryTextRows     = 50
	querySlots        = 2

	queryMaxHeld = 1_000_000
)

// querySlots is full: the tool answers busy, the message the old BusyError
// carried. The MCP layer answers it invalid_request until the 429 mapping
// returns to internal/mcp.
var querySlot = make(chan struct{}, querySlots)

type queryTool struct{}

func (queryTool) Name() string { return "query" }

type queryArgs struct {
	Query string `json:"query"`
	From  string `json:"from"`
	To    string `json:"to"`
	Limit *int   `json:"limit"`
}

func parseQueryArgs(raw json.RawMessage) (a queryArgs, from, to time.Time, limit int, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&a); err != nil {
		return a, from, to, 0, argErrorf("invalid arguments: %s", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return a, from, to, 0, argErrorf("query is required")
	}
	if len(a.Query) > queryMaxStatement {
		return a, from, to, 0, argErrorf("query is longer than %d characters", queryMaxStatement)
	}
	for _, f := range []struct {
		name, val string
		dst       *time.Time
	}{{"from", a.From, &from}, {"to", a.To, &to}} {
		if f.val == "" {
			return a, from, to, 0, argErrorf("%s is required (an RFC 3339 timestamp)", f.name)
		}
		t, perr := time.Parse(time.RFC3339, f.val)
		if perr != nil {
			return a, from, to, 0, argErrorf("%s must be an RFC 3339 timestamp: %s", f.name, perr)
		}
		*f.dst = t.UTC()
	}
	if !from.Before(to) {
		return a, from, to, 0, argErrorf("from must be before to")
	}
	if to.Sub(from) > queryMaxRange {
		return a, from, to, 0, argErrorf("the range from..to spans more than 31 days; query at most 31 days at a time")
	}
	limit = queryDefaultLimit
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > queryMaxLimit {
			return a, from, to, 0, argErrorf("limit must be between 1 and %d", queryMaxLimit)
		}
		limit = *a.Limit
	}
	return a, from, to, limit, nil
}

func (queryTool) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
	a, from, to, limit, err := parseQueryArgs(raw)
	if err != nil {
		return nil, "", err
	}
	select {
	case querySlot <- struct{}{}:
		defer func() { <-querySlot }()
	default:
		return nil, "", argErrorf("all %d concurrent query slots are in use; retry in 1 s", querySlots)
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	data, text, err := runQuery(ctx, db, a.Query, from, to, limit)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "", argErrorf("statement timeout: the query was stopped at its time limit of %d s", int(queryTimeout.Seconds()))
		}
		return nil, "", err
	}
	return data, text, nil
}

func runQuery(ctx context.Context, db *DB, q string, from, to time.Time, limit int) (any, string, error) {
	parsed, err := ql.Parse(q)
	if err != nil {
		return nil, "", argErrorf("%s", err)
	}
	plan, err := ql.Check(parsed, ql.Relations)
	if err != nil {
		return nil, "", argErrorf("%s", err)
	}
	src, err := newQLSource(ctx, db, from, to)
	if err != nil {
		return nil, "", err
	}
	res, err := ql.Run(ctx, plan, src, ql.Limits{Rows: limit, Held: queryMaxHeld})
	if err != nil {
		return nil, "", err
	}
	data := map[string]any{
		"columns":   res.Columns,
		"rows":      res.Rows,
		"row_count": len(res.Rows),
		"truncated": res.Truncated,
		"limit":     limit,
		"from":      from.Format(time.RFC3339),
		"to":        to.Format(time.RFC3339),
	}
	return data, queryText(res, truncated{truncated: res.Truncated, limit: limit}, from, to), nil
}

type truncated struct {
	truncated bool
	limit     int
}

func queryText(res ql.Result, tr truncated, from, to time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "query returned %d row(s) for %s to %s", len(res.Rows), from.Format(time.RFC3339), to.Format(time.RFC3339))
	if tr.truncated {
		fmt.Fprintf(&b, "; truncated at limit %d, narrow the statement or raise limit (at most %d)", tr.limit, queryMaxLimit)
	}
	b.WriteString(".\n")
	if len(res.Rows) == 0 {
		return b.String()
	}
	names := make([]string, len(res.Columns))
	for i, c := range res.Columns {
		names[i] = c.Name
	}
	b.WriteString(strings.Join(names, "\t") + "\n")
	for i, r := range res.Rows {
		if i == queryTextRows {
			fmt.Fprintf(&b, "... %d more row(s) in the structured result\n", len(res.Rows)-queryTextRows)
			break
		}
		cells := make([]string, len(r))
		for j, v := range r {
			if v.IsNull() {
				cells[j] = "NULL"
				continue
			}
			if s, ok := v.Any().(string); ok {
				cells[j] = s
			} else if enc, err := json.Marshal(v.Any()); err == nil {
				cells[j] = string(enc)
			} else {
				cells[j] = fmt.Sprint(v.Any())
			}
		}
		b.WriteString(strings.Join(cells, "\t") + "\n")
	}
	return b.String()
}
