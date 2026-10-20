//! One-time import of the retired ClickHouse store. Owned by lane
//! `archive`. `scripts/mnemonic/export_ch.sh` writes per-month parquet
//! exports of `moraine_personal.events`; this module splits them per
//! source file into `machine=archive-ch` using `write::write_events`.

use std::path::Path;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct ImportStats {
    pub export_files: usize,
    pub rows_read: u64,
    /// Rows whose `source_file` already has a lake file under a real
    /// machine prefix; those sources are covered by live ingest.
    pub rows_skipped_live: u64,
    pub lake_files_written: usize,
}

/// Import every `*.parquet` under `export_dir` into
/// `<lake_dir>/machine=archive-ch/...`. Idempotent: rerunning rewrites the
/// same files with the same rows.
pub fn import_exports(export_dir: &Path, lake_dir: &Path) -> anyhow::Result<ImportStats> {
    let _ = (export_dir, lake_dir);
    todo!("lane archive")
}
