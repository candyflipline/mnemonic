//! One source file's event rows to one zstd parquet file. Owned by lane
//! `writer`; the signatures below are the contract `scan` and `archive`
//! call.

use std::path::Path;

use serde_json::Value;

use crate::layout::FooterMeta;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct WriteStats {
    pub rows: usize,
    pub bytes: u64,
}

/// Write `rows` (moraine event rows, already redacted) to `out` with
/// `schema::events_schema()`, zstd level 3, footer pairs from `meta`.
/// Stamps `author` and `ingested_at` (= `meta.written_at_ms`) on every row.
/// Keeps one row per `event_uid`, the one with the highest `event_version`
/// (later row wins a tie). Writes `<out>.tmp` then renames, so `out` is
/// either the old file or the complete new one.
pub fn write_events(
    rows: &[Value],
    meta: &FooterMeta,
    author: &str,
    out: &Path,
) -> anyhow::Result<WriteStats> {
    let _ = (rows, meta, author, out);
    todo!("lane writer")
}

/// The footer of an existing lake file, or `None` when the file is missing,
/// unreadable as parquet, or carries no complete `FooterMeta`.
pub fn read_footer(path: &Path) -> anyhow::Result<Option<FooterMeta>> {
    let _ = path;
    todo!("lane writer")
}
