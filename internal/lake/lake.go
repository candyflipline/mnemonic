// Package lake turns raw sources into events parquet and imports the
// ClickHouse archive. Owned by lane lake.
package lake

type Stats struct {
	Sources, Rewrote, Unchanged, RawOnly, Failed int
	Events                                       int64
}

// Run re-parses every raw source whose manifest UpdatedMs is newer than its
// parquet (or has none) with normalize.For(harness), stamps author,
// machine_id and ingested_at, dedupes event_uid by highest event_version, and
// writes events/... via DuckDB COPY (zstd) to a temp file then rename. A
// harness without a parser counts as RawOnly. A second run rewrites 0.
func Run(dataDir string) (Stats, error) { panic("lane lake") }

// ImportClickHouse reads per-month parquet exports of moraine events (all 59
// columns) from exportDir into events/author=<author>/machine=archive-ch/,
// skipping rows whose source_file is already in the lake. Idempotent.
func ImportClickHouse(dataDir, exportDir string) (Stats, error) { panic("lane lake") }
