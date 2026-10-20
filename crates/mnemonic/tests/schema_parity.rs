//! The lake schema must hold every column the normalizer emits. Reads the
//! ingest-core golden fixtures (the normalizer's pinned output) and fails,
//! naming the fixture and key, when an event row carries a column that
//! `schema::EVENT_COLUMNS` lacks. Fix: regenerate `src/schema.rs`.

use std::collections::BTreeSet;
use std::path::Path;

use mnemonic::schema::{events_schema, EVENT_COLUMNS};

#[test]
fn every_golden_event_column_is_in_the_lake_schema() {
    let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../moraine-ingest-core/tests/goldens/source_normalization");
    let known: BTreeSet<&str> = EVENT_COLUMNS.iter().copied().collect();
    let mut missing = Vec::new();
    let mut fixtures = 0;
    for entry in std::fs::read_dir(&dir).expect("golden dir") {
        let path = entry.expect("dir entry").path();
        if path.extension().and_then(|ext| ext.to_str()) != Some("json") {
            continue;
        }
        fixtures += 1;
        let golden: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&path).expect("read golden")).expect("parse golden");
        for record in golden["records"].as_array().expect("records") {
            for row in record["event_rows"].as_array().expect("event_rows") {
                for key in row.as_object().expect("event row object").keys() {
                    if !known.contains(key.as_str()) {
                        missing.push(format!("{}: {key}", path.display()));
                    }
                }
            }
        }
    }
    assert_eq!(fixtures, 14, "golden fixture count changed; review this test");
    missing.sort();
    missing.dedup();
    assert!(missing.is_empty(), "columns missing from schema.rs: {missing:#?}");
    assert_eq!(events_schema().fields().len(), EVENT_COLUMNS.len());
}
