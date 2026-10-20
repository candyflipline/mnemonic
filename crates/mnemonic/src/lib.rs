//! mnemonic writer side: moraine's normalizer and redaction, one zstd
//! parquet file of `events` per source file, under this machine's prefix.
//!
//! Layout (`layout.rs`) and schema (`schema.rs`) are the contracts every
//! other crate reads. `scan` decides which sources to rewrite; `write` turns
//! one file's rows into parquet; `archive` imports the one-time ClickHouse
//! export.

pub mod archive;
pub mod layout;
pub mod scan;
pub mod schema;
pub mod write;
