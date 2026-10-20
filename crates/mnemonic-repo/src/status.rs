//! `get_ingest_status`: freshness of this machine's lake, from the cache's
//! `sources` table. Owned by lane `attention-status`.

use moraine_conversations::{IngestHeartbeatRead, RepoResult};

pub(crate) fn latest_ingest_heartbeat(conn: &duckdb::Connection) -> RepoResult<IngestHeartbeatRead> {
    let _ = conn;
    todo!("lane attention-status")
}
