//! Decide which sources to (re)write and write them. Owned by lane `writer`.

use std::time::SystemTime;

use moraine_config::AppConfig;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct RunStats {
    /// Source files matched by the enabled `[[ingest.sources]]` globs.
    pub scanned: usize,
    /// Parquet files written this run.
    pub rewrote: usize,
    /// Footer matches the source (size, head_hash, mtime, schema_version).
    pub unchanged: usize,
    /// Changed, but still being written to (inside `idle_seconds` and the
    /// existing parquet is younger than `max_stale_seconds`).
    pub deferred: usize,
    pub failed: usize,
}

/// One pass over every enabled jsonl source under `app.ingest.sources`,
/// writing changed files under `app.mnemonic`'s machine prefix. Files run
/// one at a time (a 262 MB transcript peaks near 1.6 GB RSS). A lake file
/// whose source is gone is kept. Errors on one file count as `failed` and
/// do not stop the pass.
pub async fn run_once(app: &AppConfig, now: SystemTime) -> anyhow::Result<RunStats> {
    let _ = (app, now);
    todo!("lane writer")
}
