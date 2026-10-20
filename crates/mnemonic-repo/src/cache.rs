//! The derived DuckDB cache. Owned by lane `cache`.
//!
//! `build` reads every machine's lake files and writes a complete new
//! `cache-<unix_ms>.duckdb` (via `.tmp` then rename), keeping the newest two
//! and deleting older ones. `Cache` hands out read-only connections to the
//! newest complete file, reopening when a newer one appears. Many processes
//! may hold read-only connections at once; nothing ever writes a cache file
//! after its rename.

use std::path::{Path, PathBuf};

use moraine_conversations::RepoResult;

/// The relations `build` must create, verbatim contract for query lanes.
pub const CACHE_SCHEMA: &str = include_str!("cache_schema.sql");

#[derive(Debug, Clone, Default, PartialEq)]
pub struct BuildStats {
    pub lake_files: usize,
    pub events: u64,
    pub docs: u64,
    pub postings: u64,
    pub seconds: f64,
    pub path: PathBuf,
}

/// Build a new cache from `lake_dir` into `cache_dir`.
pub fn build(lake_dir: &Path, cache_dir: &Path) -> anyhow::Result<BuildStats> {
    let _ = (lake_dir, cache_dir);
    todo!("lane cache")
}

pub struct Cache {
    cache_dir: PathBuf,
}

impl Cache {
    /// Does not require a cache to exist yet; `connection` reports that.
    pub fn open(cache_dir: &Path) -> anyhow::Result<Self> {
        Ok(Self {
            cache_dir: cache_dir.to_path_buf(),
        })
    }

    /// A read-only connection to the newest complete cache file. Errors
    /// with `RepoError::Backend` naming `cache_dir` when none exists.
    pub fn connection(&self) -> RepoResult<duckdb::Connection> {
        let _ = &self.cache_dir;
        todo!("lane cache")
    }
}
