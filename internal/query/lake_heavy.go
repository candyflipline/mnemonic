package query

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
)

func hydrateKept(ctx context.Context, db *sql.DB, sc *sessionScan) error {
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
	return hydrateFromLake(ctx, db, rows)
}

type heavy struct {
	payloadJSON, tokenUsageJSON string
}

// hydrateFromLake reads each row's heavy columns at its locator. A live lake
// file is rewritten whole when its source grows, so a row whose locator now
// holds another event is read again by event_uid: the file's winning version.
// A tail that lake removed before the index caught up has handed its rows to
// the source's base, so its rows are read from the base by event_uid.
func hydrateFromLake(ctx context.Context, db *sql.DB, rows []*navRow) error {
	byFile := map[string][]*navRow{}
	for _, r := range rows {
		byFile[r.lakeFile] = append(byFile[r.lakeFile], r)
	}
	for file, rs := range byFile {
		want := map[string]bool{}
		nums := make([]string, 0, len(rs))
		for _, r := range rs {
			if !want[r.eventUID] {
				want[r.eventUID] = true
				nums = append(nums, strconv.FormatInt(r.lakeRow, 10))
			}
		}
		got, err := heavyAt(ctx, db, file, nums, want)
		if base, ok := strings.CutSuffix(file, ".tail.parquet"); ok && err != nil {
			if _, serr := os.Stat(file); os.IsNotExist(serr) {
				got, err = heavyAt(ctx, db, base+".parquet", nil, want)
			}
		}
		if err != nil {
			return err
		}
		for _, r := range rs {
			h := got[r.eventUID]
			r.payloadJSON, r.tokenUsageJSON = h.payloadJSON, h.tokenUsageJSON
		}
	}
	return nil
}

// heavyAt reads want's heavy columns from file at row numbers nums, then by
// event_uid for any uid those rows did not hold.
func heavyAt(ctx context.Context, db *sql.DB, file string, nums []string, want map[string]bool) (map[string]heavy, error) {
	src := "read_parquet(" + sqlQuote(file) + ", hive_partitioning=false, file_row_number=true)"
	got := map[string]heavy{}
	if len(nums) > 0 {
		var err error
		got, err = readHeavy(ctx, db, "SELECT event_uid, payload_json, token_usage_json FROM "+src+
			" WHERE file_row_number IN ("+strings.Join(nums, ", ")+")", want)
		if err != nil {
			return nil, err
		}
	}
	var missing []string
	for uid := range want {
		if _, ok := got[uid]; !ok {
			missing = append(missing, sqlQuote(uid))
		}
	}
	if len(missing) == 0 {
		return got, nil
	}
	more, err := readHeavy(ctx, db, "SELECT event_uid, payload_json, token_usage_json FROM "+src+
		" WHERE event_uid IN ("+strings.Join(missing, ", ")+")"+
		" QUALIFY row_number() OVER (PARTITION BY event_uid ORDER BY event_version DESC, ingested_at DESC, file_row_number) = 1", want)
	if err != nil {
		return nil, err
	}
	for uid, h := range more {
		got[uid] = h
	}
	return got, nil
}

func readHeavy(ctx context.Context, db *sql.DB, q string, want map[string]bool) (map[string]heavy, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]heavy{}
	for rows.Next() {
		var uid string
		var p, t sql.NullString
		if err := rows.Scan(&uid, &p, &t); err != nil {
			return nil, err
		}
		if want[uid] {
			out[uid] = heavy{p.String, t.String}
		}
	}
	return out, rows.Err()
}
