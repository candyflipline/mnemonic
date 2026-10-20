CREATE VIEW moraine_personal.search_corpus_stats
(
    `bucket` UInt8,
    `docs` UInt64,
    `total_doc_len` UInt64
)
AS SELECT
    toUInt8(0) AS bucket,
    toUInt64(count()) AS docs,
    toUInt64(ifNull(sum(doc_len), 0)) AS total_doc_len
FROM moraine_personal.mcp_event_locator
FINAL
WHERE doc_len > 0
