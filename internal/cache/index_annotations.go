package cache

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
)

func isAnnotationFile(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/harness="+schema.AnnotationHarness+"/")
}

func splitAnnotations(changes []change) (events, annotations []change) {
	for _, ch := range changes {
		if isAnnotationFile(ch.file.path) {
			annotations = append(annotations, ch)
		} else {
			events = append(events, ch)
		}
	}
	return events, annotations
}

// applyAnnotations replaces the annotations rows of every changed lake file
// with the file's rows, in one transaction with its lake_files and sources
// rows. These files never reach lake_rows, events or postings.
func applyAnnotations(ctx context.Context, c *sql.Conn, dataDir string, changes []change) (int, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	var all, present []lakeFile
	var paths []string
	for _, ch := range changes {
		all = append(all, ch.file)
		if !ch.gone {
			present = append(present, ch.file)
			paths = append(paths, ch.file.path)
		}
	}
	rows, err := numRows(ctx, c, paths)
	if err != nil {
		return 0, err
	}
	inAll := "(" + strings.Join(quotePaths(all), ", ") + ")"
	stmts := []string{"BEGIN", "DELETE FROM annotations WHERE lake_file IN " + inAll}
	if len(present) > 0 {
		v := func(k string) string { return "json_extract_string(payload_json, '$.value." + k + "')" }
		stmts = append(stmts, `INSERT INTO annotations SELECT event_uid,
  json_extract_string(payload_json, '$.target.session_id'),
  json_extract_string(payload_json, '$.target.span_id'),
  json_extract_string(payload_json, '$.name'),
  cast(`+v("number")+` AS DOUBLE), `+v("label")+`, `+v("text")+`,
  json_extract_string(payload_json, '$.source'),
  author,
  cast(json_extract_string(payload_json, '$.ts') AS TIMESTAMPTZ) AT TIME ZONE 'UTC',
  filename, file_row_number
FROM `+parquetSource(present))
	}
	stmts = append(stmts, "DELETE FROM lake_files WHERE path IN "+inAll)
	for _, f := range present {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO lake_files VALUES (%s, %d, %d, %d)",
			sqlStr(f.path), f.size, f.mtime, rows[f.path]))
	}
	srcStmts, err := sourceRows(dataDir, changes)
	if err != nil {
		return 0, err
	}
	stmts = append(append(stmts, srcStmts...), "COMMIT")
	if err := execAll(ctx, c, stmts); err != nil {
		c.ExecContext(context.Background(), "ROLLBACK")
		return 0, err
	}
	return len(changes), nil
}
