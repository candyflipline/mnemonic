//! `file_attention`: sessions that touched a path. Owned by lane
//! `attention-status`.

use moraine_conversations::{FileAttentionQuery, FileAttentionTouch, RepoResult};

pub(crate) fn file_attention(
    conn: &duckdb::Connection,
    query: &FileAttentionQuery,
) -> RepoResult<Vec<FileAttentionTouch>> {
    let _ = (conn, query);
    todo!("lane attention-status")
}
