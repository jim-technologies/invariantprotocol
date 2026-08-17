use std::collections::HashMap;
use std::path::PathBuf;

use invariant::cdc::v1::source_extension::Representation;
use invariant::cdc::v1::value::Kind;
use invariant::cdc::v1::{
    ChangeRecord, ChangedFieldMask, DataCollection, DecimalValue, FieldPath, ListValue, NullValue,
    OpaqueData, Operation, Record, RecordField, SchemaReference, SourceExtension, SourcePosition,
    TransactionContext, Value,
};
use invariant::cloudevents::v1::CloudEvent;
use invariant::cloudevents::v1::cloud_event::cloud_event_attribute_value::Attr;
use invariant::cloudevents::v1::cloud_event::{CloudEventAttributeValue, Data};
use prost::Message;
use prost_types::{Any, Timestamp};
use serde_json::Value as JsonValue;

const EVENT_TYPE: &str = "io.invariantprotocol.cdc.v1.change";
const CHANGE_RECORD_TYPE_URL: &str = "type.googleapis.com/invariant.cdc.v1.ChangeRecord";
const FIXTURE_VERSION: &str = "3.6.1.Final";

fn timestamp(seconds: i64, nanos: i32) -> Timestamp {
    Timestamp { seconds, nanos }
}

fn value(kind: Kind) -> Value {
    Value {
        type_name: String::new(),
        kind: Some(kind),
    }
}

fn field(name: &str, value: Value) -> RecordField {
    RecordField {
        name: name.into(),
        value: Some(value),
    }
}

fn rich_change_record() -> ChangeRecord {
    ChangeRecord {
        operation: Operation::Update as i32,
        key: Some(Record {
            fields: vec![field("id", value(Kind::Uint64Value(u64::MAX)))],
        }),
        before: None,
        after: Some(Record {
            fields: vec![
                field("explicit_null", value(Kind::NullValue(NullValue {}))),
                field("unsigned", value(Kind::Uint64Value(u64::MAX))),
                field(
                    "amount",
                    Value {
                        type_name: "org.apache.kafka.connect.data.Decimal".into(),
                        kind: Some(Kind::DecimalValue(DecimalValue {
                            value: "12345678901234567890.123456789".into(),
                            scale: 9,
                            precision: Some(29),
                        })),
                    },
                ),
                field(
                    "binary",
                    value(Kind::BytesValue(b"\x00\xff\x10binary".to_vec())),
                ),
                field(
                    "occurred_at",
                    Value {
                        type_name: "io.debezium.time.NanoTimestamp".into(),
                        kind: Some(Kind::TimestampValue(timestamp(1_721_234_567, 987_654_321))),
                    },
                ),
                field(
                    "items",
                    value(Kind::ListValue(ListValue {
                        values: vec![
                            value(Kind::StringValue("first".into())),
                            value(Kind::NullValue(NullValue {})),
                            value(Kind::Uint64Value(9_007_199_254_740_993)),
                        ],
                    })),
                ),
                field(
                    "address",
                    value(Kind::RecordValue(Record {
                        fields: vec![
                            field("city", value(Kind::StringValue("Oakland".into()))),
                            field("zip", value(Kind::Uint32Value(94_607))),
                        ],
                    })),
                ),
                // "omitted" is deliberately absent: absence is not a null Value.
            ],
        }),
        data_collection: Some(DataCollection {
            id: "inventory.public.customers".into(),
        }),
        schema_reference: Some(SchemaReference {
            uri: "urn:example:schema:customers".into(),
            version: "42".into(),
            fingerprint: vec![0x12, 0x34, 0x56, 0x78],
        }),
        source_position: Some(SourcePosition {
            stream: "source-stream-7".into(),
            format: "application/vnd.debezium.source-position+json".into(),
            value: br#"{"opaque":"position","lsn":24023128}"#.to_vec(),
        }),
        transaction: Some(TransactionContext {
            id: "tx-123".into(),
            total_order: Some(9_007_199_254_740_993),
            data_collection_order: Some(7),
        }),
        source_time: Some(timestamp(1_721_234_567, 123_456_789)),
        capture_time: Some(timestamp(1_721_234_568, 1)),
        changed_fields: Some(ChangedFieldMask {
            paths: vec![
                FieldPath {
                    segments: vec!["amount".into()],
                },
                FieldPath {
                    segments: vec!["address".into(), "city".into()],
                },
            ],
        }),
        source_extension: Some(SourceExtension {
            representation: Some(Representation::OpaqueData(OpaqueData {
                media_type: "application/json".into(),
                schema: "https://debezium.io/schemas/3.6/source/postgresql".into(),
                data: br#"{"connector":"postgresql","future_source_field":{"x":1}}"#.to_vec(),
            })),
        }),
        source_message: None,
    }
}

fn attribute(attr: Attr) -> CloudEventAttributeValue {
    CloudEventAttributeValue { attr: Some(attr) }
}

fn cloud_event(record: &ChangeRecord) -> CloudEvent {
    CloudEvent {
        id: "server-1:24023128:7".into(),
        source: "urn:invariant:test:source:inventory".into(),
        spec_version: "1.0".into(),
        r#type: EVENT_TYPE.into(),
        attributes: HashMap::from([
            (
                "time".into(),
                attribute(Attr::CeTimestamp(timestamp(1_721_234_567, 123_456_789))),
            ),
            (
                "datacontenttype".into(),
                attribute(Attr::CeString("application/protobuf".into())),
            ),
            (
                "dataschema".into(),
                attribute(Attr::CeUri(CHANGE_RECORD_TYPE_URL.into())),
            ),
            (
                "correlationid".into(),
                attribute(Attr::CeString("request-42".into())),
            ),
            (
                "causationid".into(),
                attribute(Attr::CeString("command-11".into())),
            ),
            (
                "traceparent".into(),
                attribute(Attr::CeString(
                    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01".into(),
                )),
            ),
        ]),
        data: Some(Data::ProtoData(Any {
            type_url: CHANGE_RECORD_TYPE_URL.into(),
            value: record.encode_to_vec(),
        })),
    }
}

fn record_value<'a>(record: &'a Record, name: &str) -> &'a Value {
    record
        .fields
        .iter()
        .find(|field| field.name == name)
        .and_then(|field| field.value.as_ref())
        .unwrap_or_else(|| panic!("missing record field {name}"))
}

fn assert_rich_values(record: &ChangeRecord) {
    assert_eq!(record.operation, Operation::Update as i32);
    assert!(record.key.is_some());
    assert!(record.before.is_none());
    let after = record.after.as_ref().expect("after image");
    assert_eq!(
        record
            .data_collection
            .as_ref()
            .map(|value| value.id.as_str()),
        Some("inventory.public.customers")
    );
    assert_eq!(
        record
            .source_position
            .as_ref()
            .map(|value| value.value.as_slice()),
        Some(br#"{"opaque":"position","lsn":24023128}"#.as_slice())
    );
    assert_eq!(
        record
            .transaction
            .as_ref()
            .and_then(|value| value.total_order),
        Some(9_007_199_254_740_993)
    );
    assert_eq!(
        record.source_time,
        Some(timestamp(1_721_234_567, 123_456_789))
    );

    assert!(!after.fields.iter().any(|field| field.name == "omitted"));
    assert!(matches!(
        record_value(after, "explicit_null").kind,
        Some(Kind::NullValue(_))
    ));
    assert!(matches!(
        record_value(after, "unsigned").kind,
        Some(Kind::Uint64Value(u64::MAX))
    ));
    let Some(Kind::DecimalValue(decimal)) = &record_value(after, "amount").kind else {
        panic!("amount must be a decimal");
    };
    assert_eq!(
        (decimal.value.as_str(), decimal.scale, decimal.precision),
        ("12345678901234567890.123456789", 9, Some(29))
    );
    assert!(matches!(
        &record_value(after, "binary").kind,
        Some(Kind::BytesValue(bytes)) if bytes == b"\x00\xff\x10binary"
    ));
    assert!(matches!(
        &record_value(after, "occurred_at").kind,
        Some(Kind::TimestampValue(value)) if *value == timestamp(1_721_234_567, 987_654_321)
    ));

    let Some(Kind::ListValue(items)) = &record_value(after, "items").kind else {
        panic!("items must be a list");
    };
    assert!(matches!(
        &items.values[0].kind,
        Some(Kind::StringValue(value)) if value == "first"
    ));
    assert!(matches!(items.values[1].kind, Some(Kind::NullValue(_))));
    assert!(matches!(
        items.values[2].kind,
        Some(Kind::Uint64Value(9_007_199_254_740_993))
    ));

    let Some(Kind::RecordValue(address)) = &record_value(after, "address").kind else {
        panic!("address must be a nested record");
    };
    assert!(matches!(
        &record_value(address, "city").kind,
        Some(Kind::StringValue(value)) if value == "Oakland"
    ));
    assert!(matches!(
        record_value(address, "zip").kind,
        Some(Kind::Uint32Value(94_607))
    ));

    assert_eq!(
        record
            .changed_fields
            .as_ref()
            .expect("changed fields")
            .paths
            .iter()
            .map(|path| path.segments.as_slice())
            .collect::<Vec<_>>(),
        [
            ["amount".to_string()].as_slice(),
            ["address".to_string(), "city".to_string()].as_slice(),
        ]
    );
    assert!(matches!(
        record
            .source_extension
            .as_ref()
            .and_then(|value| value.representation.as_ref()),
        Some(Representation::OpaqueData(data))
            if data.data.windows(b"future_source_field".len()).any(|window| window == b"future_source_field")
    ));
}

#[test]
fn cloud_event_wraps_typed_change_record_without_loss() {
    let event = cloud_event(&rich_change_record());
    let decoded = CloudEvent::decode(event.encode_to_vec().as_slice()).expect("CloudEvent");

    assert_eq!(
        (decoded.source.as_str(), decoded.id.as_str()),
        ("urn:invariant:test:source:inventory", "server-1:24023128:7")
    );
    assert_eq!(decoded.spec_version, "1.0");
    assert_eq!(decoded.r#type, EVENT_TYPE);
    assert!(matches!(
        decoded
            .attributes
            .get("datacontenttype")
            .and_then(|value| value.attr.as_ref()),
        Some(Attr::CeString(value)) if value == "application/protobuf"
    ));
    assert!(matches!(
        decoded
            .attributes
            .get("dataschema")
            .and_then(|value| value.attr.as_ref()),
        Some(Attr::CeUri(value)) if value == CHANGE_RECORD_TYPE_URL
    ));
    assert!(matches!(
        decoded
            .attributes
            .get("time")
            .and_then(|value| value.attr.as_ref()),
        Some(Attr::CeTimestamp(value)) if *value == timestamp(1_721_234_567, 123_456_789)
    ));

    let Some(Data::ProtoData(proto_data)) = &decoded.data else {
        panic!("CDC payload must use CloudEvent.proto_data");
    };
    assert_eq!(proto_data.type_url, CHANGE_RECORD_TYPE_URL);
    let record = ChangeRecord::decode(proto_data.value.as_slice()).expect("ChangeRecord");
    assert_rich_values(&record);

    let mut retry_record = rich_change_record();
    retry_record.capture_time = Some(timestamp(1_721_234_569, 2));
    let retry = cloud_event(&retry_record);
    assert_eq!((retry.source, retry.id), (decoded.source, decoded.id));
}

#[test]
fn operation_presence_shapes_are_unambiguous() {
    let cases = [
        (Operation::Create, true, false, true, true, false),
        (Operation::Update, true, false, true, true, false),
        (Operation::Delete, true, true, false, true, false),
        (Operation::SnapshotRead, true, false, true, true, false),
        (Operation::Truncate, false, false, false, true, false),
        (Operation::SourceMessage, false, false, false, false, true),
    ];
    let image = Record {
        fields: vec![field("id", value(Kind::Uint64Value(1)))],
    };

    for (operation, has_key, has_before, has_after, has_collection, has_message) in cases {
        let record = ChangeRecord {
            operation: operation as i32,
            key: has_key.then(|| image.clone()),
            before: has_before.then(|| image.clone()),
            after: has_after.then(|| image.clone()),
            data_collection: has_collection.then(|| DataCollection {
                id: "inventory.public.customers".into(),
            }),
            source_message: has_message
                .then(|| value(Kind::BytesValue(b"source-native-message".to_vec()))),
            ..Default::default()
        };
        let decoded = ChangeRecord::decode(record.encode_to_vec().as_slice()).expect("shape");
        assert_eq!(decoded.key.is_some(), has_key);
        assert_eq!(decoded.before.is_some(), has_before);
        assert_eq!(decoded.after.is_some(), has_after);
        assert_eq!(decoded.data_collection.is_some(), has_collection);
        assert_eq!(decoded.source_message.is_some(), has_message);
        if operation == Operation::SourceMessage {
            assert!(decoded.key.is_none() && decoded.before.is_none() && decoded.after.is_none());
        }
    }
}

#[test]
fn old_prost_reader_accepts_future_change_record_fields() {
    let record = rich_change_record();
    let mut future_wire = record.encode_to_vec();
    // Field 100, varint value 1, stands in for a field added by a future writer.
    future_wire.extend_from_slice(&[0xa0, 0x06, 0x01]);

    let decoded = ChangeRecord::decode(future_wire.as_slice()).expect("forward-compatible wire");
    // Prost intentionally discards unknown fields; acceptance and known semantics
    // are the compatibility guarantee available through generated Rust messages.
    assert_rich_values(&decoded);
}

fn fixture_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../testdata/cdc/debezium")
        .join(FIXTURE_VERSION)
}

fn data_change_operation(document: &JsonValue) -> Option<&str> {
    if let Some(operation) = document.get("iodebeziumop").and_then(JsonValue::as_str) {
        return Some(operation);
    }
    let mut value = document.get("value")?;
    if let Some(payload) = value.get("payload") {
        value = payload;
    }
    value.get("op").and_then(JsonValue::as_str)
}

#[test]
fn pinned_debezium_fixtures_parse_and_classify() {
    let root = fixture_root();
    let manifest: JsonValue =
        serde_json::from_slice(&std::fs::read(root.join("manifest.json")).expect("manifest"))
            .expect("valid manifest JSON");
    assert_eq!(manifest["debezium_version"], FIXTURE_VERSION);
    assert_eq!(manifest["cloudevents_specification_version"], "1.0.2");
    assert_eq!(manifest["cloudevents_event_specversion"], "1.0");

    let fixtures = manifest["fixtures"].as_array().expect("fixture entries");
    let expected_operations = [
        ("native-create-schemaful.json", "c"),
        ("native-update-schemaless.json", "u"),
        ("native-delete-schemaless.json", "d"),
        ("native-snapshot-schemaless.json", "r"),
        ("native-truncate-schemaless.json", "t"),
        ("native-logical-message-schemaless.json", "m"),
        ("structured-cloudevent-snapshot.json", "r"),
        ("structured-cloudevent-snapshot-retry.json", "r"),
    ];
    for (name, operation) in expected_operations {
        let entry = fixtures
            .iter()
            .find(|entry| entry["path"] == name)
            .unwrap_or_else(|| panic!("missing fixture {name}"));
        assert_eq!(entry["category"], "data_change");
        let document: JsonValue = serde_json::from_slice(
            &std::fs::read(root.join(name)).unwrap_or_else(|error| panic!("{name}: {error}")),
        )
        .unwrap_or_else(|error| panic!("{name}: {error}"));
        assert_eq!(data_change_operation(&document), Some(operation));
    }

    let schemaful: JsonValue = serde_json::from_slice(
        &std::fs::read(root.join("native-create-schemaful.json")).expect("schemaful fixture"),
    )
    .expect("schemaful JSON");
    assert!(schemaful["value"].get("schema").is_some());
    assert!(schemaful["value"].get("payload").is_some());
    let schemaless: JsonValue = serde_json::from_slice(
        &std::fs::read(root.join("native-update-schemaless.json")).expect("schemaless fixture"),
    )
    .expect("schemaless JSON");
    assert!(schemaless["value"].get("schema").is_none());

    let structured = std::fs::read(root.join("structured-cloudevent-snapshot.json"))
        .expect("structured fixture");
    let retry = std::fs::read(root.join("structured-cloudevent-snapshot-retry.json"))
        .expect("structured retry fixture");
    assert_eq!(structured, retry);
    let structured_json: JsonValue = serde_json::from_slice(&structured).expect("structured JSON");
    assert_eq!(structured_json["specversion"], "1.0");

    let auxiliary = [
        ("auxiliary-kafka-tombstone.json", "kafka_tombstone"),
        ("auxiliary-heartbeat.json", "heartbeat"),
        ("auxiliary-schema-change.json", "schema_change"),
        ("auxiliary-transaction-begin.json", "transaction_boundary"),
        ("auxiliary-transaction-end.json", "transaction_boundary"),
    ];
    for (name, category) in auxiliary {
        let entry = fixtures
            .iter()
            .find(|entry| entry["path"] == name)
            .unwrap_or_else(|| panic!("missing fixture {name}"));
        assert_eq!(entry["category"], category);
        assert_eq!(entry["row_change"], false);
        assert!(entry["operation"].is_null());
        let _: JsonValue = serde_json::from_slice(
            &std::fs::read(root.join(name)).unwrap_or_else(|error| panic!("{name}: {error}")),
        )
        .unwrap_or_else(|error| panic!("{name}: {error}"));
    }
}
