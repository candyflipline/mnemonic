//! Where lake files live and what their footers carry. Every reader and
//! writer derives paths from here; nothing else builds a lake path.
//!
//! `<lake_dir>/machine=<machine_id>/harness=<harness>/events/src=<source_id>.parquet`
//!
//! One writer per `machine=` prefix, so machines never share a file. A
//! source file maps to exactly one parquet file, rewritten whole (temp file,
//! then rename) when the source changes.

use std::fs::File;
use std::io::{BufRead, BufReader, Read};
use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

/// Glob, relative to `lake_dir`, matching every machine's events files.
pub const EVENTS_GLOB: &str = "machine=*/harness=*/events/*.parquet";

/// Prefix for this crate's parquet footer key-value metadata.
pub const FOOTER_PREFIX: &str = "mnemonic.";

/// Bump when the lake `events` schema changes; a reader or writer that sees
/// an older value treats the file as stale and rewrites it.
pub const LAKE_SCHEMA_VERSION: u32 = 1;

/// Machine prefix used for rows imported from the retired ClickHouse store.
pub const ARCHIVE_MACHINE_ID: &str = "archive-ch";

/// What a parquet file was written from. Stored in the footer so a rerun can
/// decide "unchanged" without any checkpoint table.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FooterMeta {
    pub source_file: String,
    pub source_name: String,
    pub harness: String,
    pub machine_id: String,
    /// Source size in bytes when it was read.
    pub size: u64,
    /// `head_hash` of the source when it was read; a different value means
    /// the file was replaced or rewritten, not appended to.
    pub head_hash: String,
    /// Source mtime, nanoseconds since the Unix epoch.
    pub mtime_ns: u64,
    /// When this parquet file was written, milliseconds since the epoch.
    pub written_at_ms: u64,
    pub schema_version: u32,
}

impl FooterMeta {
    /// Key-value pairs for the parquet footer, keys prefixed with
    /// `FOOTER_PREFIX`.
    pub fn to_pairs(&self) -> Vec<(String, String)> {
        let field = |name: &str, value: String| (format!("{FOOTER_PREFIX}{name}"), value);
        vec![
            field("source_file", self.source_file.clone()),
            field("source_name", self.source_name.clone()),
            field("harness", self.harness.clone()),
            field("machine_id", self.machine_id.clone()),
            field("size", self.size.to_string()),
            field("head_hash", self.head_hash.clone()),
            field("mtime_ns", self.mtime_ns.to_string()),
            field("written_at_ms", self.written_at_ms.to_string()),
            field("schema_version", self.schema_version.to_string()),
        ]
    }

    /// Inverse of `to_pairs`. `None` when any key is missing or malformed.
    pub fn from_pairs<'a>(pairs: impl IntoIterator<Item = (&'a str, &'a str)>) -> Option<Self> {
        let map: std::collections::HashMap<&str, &str> = pairs
            .into_iter()
            .filter_map(|(key, value)| key.strip_prefix(FOOTER_PREFIX).map(|k| (k, value)))
            .collect();
        let text = |name: &str| map.get(name).map(|value| value.to_string());
        let number = |name: &str| map.get(name).and_then(|value| value.parse::<u64>().ok());
        Some(Self {
            source_file: text("source_file")?,
            source_name: text("source_name")?,
            harness: text("harness")?,
            machine_id: text("machine_id")?,
            size: number("size")?,
            head_hash: text("head_hash")?,
            mtime_ns: number("mtime_ns")?,
            written_at_ms: number("written_at_ms")?,
            schema_version: u32::try_from(number("schema_version")?).ok()?,
        })
    }
}

/// First 16 hex chars of sha256(source path). Stable per path, short
/// enough for a file name.
pub fn source_id(source_file: &str) -> String {
    let digest = Sha256::digest(source_file.as_bytes());
    digest.iter().take(8).map(|byte| format!("{byte:02x}")).collect()
}

pub fn events_path(lake_dir: &Path, machine_id: &str, harness: &str, source_file: &str) -> PathBuf {
    lake_dir
        .join(format!("machine={machine_id}"))
        .join(format!("harness={harness}"))
        .join("events")
        .join(format!("src={}.parquet", source_id(source_file)))
}

/// sha256 hex of the source's first line, capped at 64 KiB. An empty file
/// hashes the empty string.
pub fn head_hash(source_file: &Path) -> std::io::Result<String> {
    let mut reader = BufReader::new(File::open(source_file)?).take(64 * 1024);
    let mut first = Vec::new();
    reader.read_until(b'\n', &mut first)?;
    let digest = Sha256::digest(&first);
    Ok(digest.iter().map(|byte| format!("{byte:02x}")).collect())
}
