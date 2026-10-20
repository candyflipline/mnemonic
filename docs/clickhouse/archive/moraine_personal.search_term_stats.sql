CREATE VIEW moraine_personal.search_term_stats
(
    `term` String,
    `docs` UInt64
)
AS SELECT
    term,
    toUInt64(count()) AS docs
FROM moraine_personal.search_postings
FINAL
GROUP BY term
