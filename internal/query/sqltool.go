package query

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

func init() { register(sqlTool{}) }

const (
	sqlDefaultLimit = 100
	sqlMaxLimit     = 10000
	sqlMaxStatement = 20000
	sqlMaxRange     = 31 * 24 * time.Hour
	sqlTimeout      = 30 * time.Second
	sqlTextRows     = 50
	sqlSlots        = 2
)

// BusyError is returned when every concurrent query slot is taken; the MCP
// handler answers it with HTTP 429 and a Retry-After header.
type BusyError struct{ RetryAfter time.Duration }

func (e *BusyError) Error() string {
	return fmt.Sprintf("all %d concurrent query slots are in use; retry in %d s", sqlSlots, int(e.RetryAfter.Seconds()))
}

var sqlSlot = make(chan struct{}, sqlSlots)

type sqlTool struct{}

func (sqlTool) Name() string { return "query" }

type sqlArgs struct {
	SQL   string `json:"sql"`
	From  string `json:"from"`
	To    string `json:"to"`
	Limit *int   `json:"limit"`
}

func parseSQLArgs(raw json.RawMessage) (a sqlArgs, from, to time.Time, limit int, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&a); err != nil {
		return a, from, to, 0, argErrorf("invalid arguments: %s", err)
	}
	if strings.TrimSpace(a.SQL) == "" {
		return a, from, to, 0, argErrorf("sql is required")
	}
	if len(a.SQL) > sqlMaxStatement {
		return a, from, to, 0, argErrorf("sql is longer than %d characters", sqlMaxStatement)
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
	if to.Sub(from) > sqlMaxRange {
		return a, from, to, 0, argErrorf("the range from..to spans more than 31 days; query at most 31 days at a time")
	}
	limit = sqlDefaultLimit
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > sqlMaxLimit {
			return a, from, to, 0, argErrorf("limit must be between 1 and %d", sqlMaxLimit)
		}
		limit = *a.Limit
	}
	return a, from, to, limit, nil
}

func (sqlTool) Call(ctx context.Context, db *sql.DB, raw json.RawMessage) (any, string, error) {
	a, from, to, limit, err := parseSQLArgs(raw)
	if err != nil {
		return nil, "", err
	}
	select {
	case sqlSlot <- struct{}{}:
		defer func() { <-sqlSlot }()
	default:
		return nil, "", &BusyError{RetryAfter: time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, sqlTimeout)
	defer cancel()
	cols, rows, err := runSQL(ctx, db, a.SQL, from, to, limit+1)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "", argErrorf("statement timeout: the query was stopped at its time limit of %d s", int(sqlTimeout.Seconds()))
		}
		return nil, "", sqlFailure(err)
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	data := map[string]any{
		"columns":   cols,
		"rows":      rows,
		"row_count": len(rows),
		"truncated": truncated,
		"limit":     limit,
		"from":      from.Format(time.RFC3339),
		"to":        to.Format(time.RFC3339),
	}
	return data, sqlText(cols, rows, truncated, limit, from, to), nil
}

type sqlColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// sqlFailure is a caller error when DuckDB refused the statement (it does
// not bind, a cast fails, memory ran out), and a server failure otherwise.
func sqlFailure(err error) error {
	var de *duckdb.Error
	if errors.As(err, &de) {
		switch de.Type {
		case duckdb.ErrorTypeInternal, duckdb.ErrorTypeFatal, duckdb.ErrorTypeIO, duckdb.ErrorTypeConnection, duckdb.ErrorTypeNetwork:
		default:
			return argErrorf("%s", de.Msg)
		}
	}
	return err
}

// runSQL runs stmt over the five views scoped to the caller and the range,
// on one connection whose temp views shadow the real tables, and returns at
// most n rows.
func runSQL(ctx context.Context, db *sql.DB, stmt string, from, to time.Time, n int) ([]sqlColumn, [][]any, error) {
	f, err := Sessions(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer closeQueryConn(c)
	run, err := sqlGuard(ctx, c, stmt)
	if err != nil {
		return nil, nil, err
	}
	var catalog string
	if err := c.QueryRowContext(ctx, "SELECT current_database()").Scan(&catalog); err != nil {
		return nil, nil, err
	}
	for _, ddl := range sqlScopeDDL(f, catalog, from, to) {
		if _, err := c.ExecContext(ctx, ddl); err != nil {
			return nil, nil, err
		}
	}
	rs, err := c.QueryContext(ctx, fmt.Sprintf("SELECT * FROM (%s\n) LIMIT %d", run, n))
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	types, err := rs.ColumnTypes()
	if err != nil {
		return nil, nil, err
	}
	cols := make([]sqlColumn, len(types))
	for i, t := range types {
		cols[i] = sqlColumn{Name: t.Name(), Type: t.DatabaseTypeName()}
	}
	var out [][]any
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		for i, v := range vals {
			vals[i] = jsonValue(v)
		}
		out = append(out, vals)
	}
	return cols, out, rs.Err()
}

const sqlScopeTable = "_scope"

var sqlDrop = func() []string {
	var out []string
	for _, v := range sqlViews {
		out = append(out, "DROP VIEW IF EXISTS temp.main."+v)
	}
	return append(out, "DROP TABLE IF EXISTS temp.main."+sqlScopeTable)
}()

// closeQueryConn drops the temp objects and returns the connection to the
// pool; a connection that cannot be cleaned is discarded.
func closeQueryConn(c *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ddl := range sqlDrop {
		if _, err := c.ExecContext(ctx, ddl); err != nil {
			_ = c.Raw(func(any) error { return driver.ErrBadConn })
			break
		}
	}
	c.Close()
}

const internalEventColumns = "doc, row_hash, flags, doc_len, text_hash, uid_key, lake_file, lake_row, emission_index, tool_io_input_json, meta_json"

// sqlScopeDDL creates, in order, the table of sessions the caller may list
// and the five views over the real tables. The table is built first, from
// the real tables, because the scope predicates name events unqualified.
// The views name the real tables by catalog, because main alone also names
// the temp schema, and filter to the caller's sessions and the range.
func sqlScopeDDL(f SessionFilter, catalog string, from, to time.Time) []string {
	main := `"` + strings.ReplaceAll(catalog, `"`, `""`) + `".main.`
	pred := f.Listed("s.session_id")
	if pred == "" {
		pred = "true"
	}
	const layout = "2006-01-02 15:04:05.000000"
	lo := "TIMESTAMP '" + from.Format(layout) + "'"
	hi := "TIMESTAMP '" + to.Format(layout) + "'"
	inScope := "session_id IN (SELECT session_id FROM temp.main." + sqlScopeTable + ")"
	author := f.Authors("author")
	if author == "" {
		author = "true"
	}
	return []string{
		"CREATE TEMP TABLE " + sqlScopeTable + " AS SELECT s.session_id FROM " + main + "sessions s WHERE " + pred,
		"CREATE TEMP VIEW events AS SELECT * EXCLUDE (" + internalEventColumns + ") FROM " + main + "events WHERE " + published +
			" AND event_ts >= " + lo + " AND event_ts < " + hi + " AND " + inScope,
		"CREATE TEMP VIEW spans AS SELECT * EXCLUDE (tenant) FROM " + main + "spans WHERE start_time >= " + lo + " AND start_time < " + hi + " AND " + inScope,
		"CREATE TEMP VIEW sessions AS SELECT * FROM " + main + "sessions WHERE first_event_time < " + hi + " AND last_event_time >= " + lo + " AND " + inScope,
		"CREATE TEMP VIEW turns AS SELECT * FROM " + main + "turns WHERE " + inScope +
			" AND session_id IN (SELECT session_id FROM " + main + "sessions WHERE first_event_time < " + hi + " AND last_event_time >= " + lo + ")",
		"CREATE TEMP VIEW annotations AS SELECT * EXCLUDE (lake_file, lake_row) FROM " + main + "annotations WHERE ts >= " + lo + " AND ts < " + hi +
			" AND target_session_id IN (SELECT session_id FROM temp.main." + sqlScopeTable + ") AND " + author,
	}
}

// jsonValue turns a scanned DuckDB value into one encoding/json can encode.
func jsonValue(v any) any {
	switch v := v.(type) {
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case duckdb.Decimal:
		return v.String()
	case *big.Int:
		if v.IsInt64() {
			return v.Int64()
		}
		return v.String()
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Sprint(v)
		}
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Sprint(v)
		}
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = jsonValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = jsonValue(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[fmt.Sprint(k)] = jsonValue(e)
		}
		return out
	case duckdb.Map:
		return jsonValue(map[any]any(v))
	case duckdb.UUID:
		return v.String()
	}
	return v
}

func sqlText(cols []sqlColumn, rows [][]any, truncated bool, limit int, from, to time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "query returned %d row(s) for %s to %s", len(rows), from.Format(time.RFC3339), to.Format(time.RFC3339))
	if truncated {
		fmt.Fprintf(&b, "; truncated at limit %d, narrow the statement or raise limit (at most %d)", limit, sqlMaxLimit)
	}
	b.WriteString(".\n")
	if len(rows) == 0 {
		return b.String()
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	b.WriteString(strings.Join(names, "\t") + "\n")
	for i, r := range rows {
		if i == sqlTextRows {
			fmt.Fprintf(&b, "... %d more row(s) in the structured result\n", len(rows)-sqlTextRows)
			break
		}
		cells := make([]string, len(r))
		for j, v := range r {
			if v == nil {
				cells[j] = "NULL"
			} else if s, ok := v.(string); ok {
				cells[j] = s
			} else if enc, err := json.Marshal(v); err == nil {
				cells[j] = string(enc)
			} else {
				cells[j] = fmt.Sprint(v)
			}
		}
		b.WriteString(strings.Join(cells, "\t") + "\n")
	}
	return b.String()
}
