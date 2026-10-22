package cache

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/store"
)

func isAnnotationFile(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/machine="+store.ServerMachine+"/harness="+schema.AnnotationHarness+"/")
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

// applyAnnotations applies each batch with applyAnnotationBatch and holds a
// failed batch's files back the way apply does.
func (ix *Index) applyAnnotations(ctx context.Context, c *sql.Conn, bs [][]change) (int, []error) {
	var n int
	var errs []error
	for _, b := range bs {
		err := applyAnnotationBatch(ctx, c, ix.dataDir, b)
		for _, ch := range b {
			if err != nil {
				ix.failed[ch.file.path] = ch.file
			} else {
				delete(ix.failed, ch.file.path)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("annotations from %s: %w", b[0].file.path, err))
			continue
		}
		n += len(b)
	}
	return n, errs
}

// applyAnnotationBatch replaces the annotations rows of every changed lake
// file with the file's rows, in one transaction with its lake_files and
// sources rows. These files never reach lake_rows, events or postings.
func applyAnnotationBatch(ctx context.Context, c *sql.Conn, dataDir string, changes []change) error {
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
		return err
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
		return err
	}
	stmts = append(append(stmts, srcStmts...), "COMMIT")
	if err := execAll(ctx, c, stmts); err != nil {
		c.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	return nil
}
