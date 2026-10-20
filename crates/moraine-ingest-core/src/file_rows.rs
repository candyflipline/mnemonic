//! Whole-file normalization for stores that rewrite one output per source
//! file (mnemonic's parquet lake). It runs the same `process_file` walk the
//! ingest dispatcher uses, from offset zero with no stored checkpoint, and
//! applies the default-backend secret redaction, so a caller gets exactly the
//! rows moraine would have written for that file.

use std::collections::HashMap;
use std::sync::Arc;

use anyhow::{Context, Result};
use moraine_config::{AppConfig, SourceFormat};
use tokio::sync::{mpsc, RwLock};

use crate::dispatch::process_file;
use crate::model::{Checkpoint, RowBatch};
use crate::redaction::SecretRedactor;
use crate::sqlite_poll::VolatilePollMap;
use crate::{Metrics, SinkMessage, WorkItem};

/// Rows for one source file, already redacted unless the config opts out.
#[derive(Debug, Default)]
pub struct FileRows {
    pub batch: RowBatch,
    /// Checkpoint `process_file` reached: the byte offset and line number
    /// the rows cover.
    pub checkpoint: Option<Checkpoint>,
}

/// Normalize `path` from its first byte. `format` must be resolved
/// (not `SourceFormat::Infer`).
pub async fn normalize_file(
    config: &AppConfig,
    source_name: &str,
    harness: &str,
    format: SourceFormat,
    path: &str,
) -> Result<FileRows> {
    let work = WorkItem {
        source_name: source_name.to_string(),
        harness: harness.to_string(),
        format,
        source_glob: String::new(),
        path: path.to_string(),
    };
    let checkpoints = Arc::new(RwLock::new(HashMap::<String, Checkpoint>::new()));
    let metrics = Arc::new(Metrics::default());
    let poll_state = VolatilePollMap::new();
    let (sink_tx, mut sink_rx) = mpsc::channel::<SinkMessage>(64);

    let drain = async {
        let mut out = FileRows::default();
        while let Some(message) = sink_rx.recv().await {
            if let SinkMessage::Batch(mut batch) = message {
                if let Some(checkpoint) = batch.checkpoint.take() {
                    out.checkpoint = Some(checkpoint);
                }
                out.batch.raw_rows.append(&mut batch.raw_rows);
                out.batch.event_rows.append(&mut batch.event_rows);
                out.batch.link_rows.append(&mut batch.link_rows);
                out.batch.tool_rows.append(&mut batch.tool_rows);
                out.batch.error_rows.append(&mut batch.error_rows);
                out.batch.lines_processed += batch.lines_processed;
            }
        }
        out
    };
    let walk = async {
        let result = process_file(config, &work, checkpoints, &poll_state, sink_tx, &metrics).await;
        result.with_context(|| format!("normalize {path}"))
    };
    let (walked, mut out) = tokio::join!(walk, drain);
    walked?;

    if !config.redaction.dangerously_skip_secret_redaction {
        SecretRedactor::new(&config.redaction)?.redact_batch(&mut out.batch);
    }
    Ok(out)
}
