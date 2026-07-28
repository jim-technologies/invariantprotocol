use invariant::data::v1::{
    DataType, DatasetSchema, DecimalType, Field, FixedBytesType, ListType, Presence, PrimitiveKind,
    PrimitiveType, RetiredField, SchemaBundle, SyntheticRole, UuidType, data_type,
};
use invariant::{
    DataSchemaError, SCHEMA_IR_VERSION, SCHEMA_MAPPING_VERSION, migrate_schema_bundle,
    parse_schema_bundle,
};
use prost::Message;

#[test]
fn reads_shared_canonical_schema_bundle() {
    let encoded = include_bytes!("../../testdata/data.schema.binpb");
    let bundle = parse_schema_bundle(encoded.as_slice()).expect("valid canonical schema bundle");

    assert_eq!(bundle.ir_version, SCHEMA_IR_VERSION);
    assert_eq!(bundle.mapping_version, SCHEMA_MAPPING_VERSION);
    assert_eq!((bundle.ir_version, bundle.mapping_version), (4, 3));
    assert_eq!(
        bundle
            .datasets
            .iter()
            .map(|dataset| dataset.source_message.as_str())
            .collect::<Vec<_>>(),
        ["data.v1.CanonicalRecord", "data.v1.Proto2Record"]
    );

    let canonical = dataset(&bundle, "data.v1.CanonicalRecord");
    let optional_note = field(&canonical.fields, "optional_note");
    assert_eq!(
        (
            optional_note.stable_id,
            optional_note.presence,
            optional_note.nullable
        ),
        (17, Presence::Explicit as i32, true)
    );
    assert_eq!(optional_note.storage_name_source, "optional_note");

    let labels = field(&canonical.fields, "labels");
    assert_eq!(
        (labels.stable_id, labels.presence),
        (19, Presence::Repeated as i32)
    );
    let Some(data_type::Kind::List(list)) = labels
        .r#type
        .as_deref()
        .and_then(|data_type| data_type.kind.as_ref())
    else {
        panic!("labels must have a canonical list type");
    };
    let element = list.element.as_deref().expect("list element");
    assert_eq!(element.stable_id, 31);
    assert_eq!(element.presence, Presence::NotApplicable as i32);
    assert_eq!(element.synthetic_role, SyntheticRole::ListElement as i32);
    assert!(element.storage_name_source.is_empty());

    let choice_count = field(&canonical.fields, "choice_count");
    assert_eq!(
        (
            choice_count.stable_id,
            choice_count.presence,
            choice_count.oneof.as_str()
        ),
        (22, Presence::Oneof as i32, "choice")
    );

    let proto2 = dataset(&bundle, "data.v1.Proto2Record");
    let id = field(&proto2.fields, "id");
    assert_eq!((id.stable_id, id.presence), (1, Presence::Required as i32));
    let label = field(&proto2.fields, "label");
    assert_eq!(
        (
            label.stable_id,
            label.presence,
            label.has_default,
            label.protobuf_default.as_str()
        ),
        (2, Presence::Explicit as i32, true, "unknown")
    );

    assert_eq!(bundle.encode_to_vec(), encoded.as_slice());
}

#[test]
fn rejects_unsupported_bundle_versions() {
    let mut bundle = SchemaBundle {
        ir_version: 1,
        mapping_version: SCHEMA_MAPPING_VERSION,
        ..Default::default()
    };
    assert!(matches!(
        invariant::validate_schema_bundle(&bundle),
        Err(DataSchemaError::UnsupportedIrVersion { .. })
    ));

    bundle.ir_version = SCHEMA_IR_VERSION;
    bundle.mapping_version = 1;
    assert!(matches!(
        invariant::validate_schema_bundle(&bundle),
        Err(DataSchemaError::UnsupportedMappingVersion { .. })
    ));
}

#[test]
fn migrates_exact_legacy_version_without_losing_schema_state() {
    let legacy = SchemaBundle {
        ir_version: 3,
        mapping_version: 2,
        source_descriptor_sha256: b"digest".to_vec(),
        datasets: vec![DatasetSchema {
            source_message: "example.v1.Record".into(),
            name: "example_v1_record".into(),
            last_field_id: 8,
            fields: vec![Field {
                name: "values".into(),
                stable_id: 7,
                r#type: Some(Box::new(DataType {
                    kind: Some(data_type::Kind::List(Box::new(ListType {
                        element: Some(Box::new(Field {
                            name: "element".into(),
                            stable_id: 8,
                            r#type: Some(Box::new(DataType {
                                kind: Some(data_type::Kind::Primitive(PrimitiveType {
                                    kind: PrimitiveKind::Float as i32,
                                })),
                                ..Default::default()
                            })),
                            ..Default::default()
                        })),
                        fixed_length: 0,
                    }))),
                    ..Default::default()
                })),
                ..Default::default()
            }],
            retired_fields: vec![RetiredField {
                identity: "f:6".into(),
                stable_id: 6,
                proto_full_name: "example.v1.Record.old_value".into(),
                name: "old_value".into(),
                storage_name_source: "old_value".into(),
            }],
            ..Default::default()
        }],
    };

    let migrated = migrate_schema_bundle(legacy.clone()).expect("supported legacy bundle");

    assert_eq!(
        (migrated.ir_version, migrated.mapping_version),
        (SCHEMA_IR_VERSION, SCHEMA_MAPPING_VERSION)
    );
    assert_eq!(migrated.source_descriptor_sha256, b"digest");
    let dataset = &migrated.datasets[0];
    assert_eq!(dataset.last_field_id, 8);
    assert_eq!(dataset.fields[0].stable_id, 7);
    assert_eq!(dataset.retired_fields[0].stable_id, 6);
    assert_eq!(dataset.retired_fields[0].identity, "f:6");
    assert_eq!(
        parse_schema_bundle(&legacy.encode_to_vec()).expect("parse migrates legacy"),
        migrated
    );
    assert_eq!(
        migrate_schema_bundle(migrated.clone()).expect("current migration is idempotent"),
        migrated
    );
}

#[test]
fn migration_rejects_mixed_version_pairs_and_legacy_fixed_lists() {
    for (ir_version, mapping_version) in [(3, 3), (4, 2)] {
        assert!(matches!(
            migrate_schema_bundle(SchemaBundle {
                ir_version,
                mapping_version,
                ..Default::default()
            }),
            Err(DataSchemaError::UnsupportedVersionPair { .. })
        ));
    }

    let legacy = SchemaBundle {
        ir_version: 3,
        mapping_version: 2,
        datasets: vec![DatasetSchema {
            name: "example_v1_record".into(),
            fields: vec![Field {
                name: "embedding".into(),
                r#type: Some(Box::new(DataType {
                    kind: Some(data_type::Kind::List(Box::new(ListType {
                        element: Some(Box::new(Field {
                            name: "element".into(),
                            ..Default::default()
                        })),
                        fixed_length: 8,
                    }))),
                    ..Default::default()
                })),
                ..Default::default()
            }],
            ..Default::default()
        }],
        ..Default::default()
    };

    assert!(matches!(
        migrate_schema_bundle(legacy),
        Err(DataSchemaError::LegacyFixedLength { ref path, length: 8 })
            if path == "example_v1_record.embedding"
    ));

    let mut unknown = SchemaBundle {
        ir_version: 3,
        mapping_version: 2,
        ..Default::default()
    }
    .encode_to_vec();
    // Unknown top-level field 127, encoded as a varint. The typed prost
    // message would discard it, so the raw parser must reject it first.
    unknown.extend_from_slice(&[0xf8, 0x07, 0x01]);
    assert!(matches!(
        parse_schema_bundle(&unknown),
        Err(DataSchemaError::LegacyUnknownFields)
    ));

    let mut nested = DatasetSchema {
        name: "example".into(),
        ..Default::default()
    }
    .encode_to_vec();
    nested.extend_from_slice(&[0xf8, 0x07, 0x01]);
    assert!(nested.len() < 128);
    let mut unknown_nested = SchemaBundle {
        ir_version: 3,
        mapping_version: 2,
        ..Default::default()
    }
    .encode_to_vec();
    unknown_nested.extend_from_slice(&[0x22, nested.len() as u8]);
    unknown_nested.extend_from_slice(&nested);
    assert!(matches!(
        parse_schema_bundle(&unknown_nested),
        Err(DataSchemaError::LegacyUnknownFields)
    ));
}

#[test]
fn round_trips_portable_refined_types_and_fixed_list_shape() {
    let refined = [
        data_type::Kind::Decimal(DecimalType {
            precision: 18,
            scale: 4,
        }),
        data_type::Kind::Uuid(UuidType {}),
        data_type::Kind::FixedBytes(FixedBytesType { byte_length: 32 }),
    ];
    let bundle = SchemaBundle {
        ir_version: SCHEMA_IR_VERSION,
        mapping_version: SCHEMA_MAPPING_VERSION,
        datasets: vec![DatasetSchema {
            source_message: "example.v1.Record".into(),
            name: "example_v1_record".into(),
            fields: refined
                .into_iter()
                .enumerate()
                .map(|(index, kind)| Field {
                    name: format!("field_{index}"),
                    r#type: Some(Box::new(DataType {
                        kind: Some(kind),
                        ..Default::default()
                    })),
                    ..Default::default()
                })
                .chain(std::iter::once(Field {
                    name: "embedding".into(),
                    r#type: Some(Box::new(DataType {
                        kind: Some(data_type::Kind::List(Box::new(ListType {
                            element: Some(Box::new(Field {
                                name: "element".into(),
                                r#type: Some(Box::new(DataType {
                                    kind: Some(data_type::Kind::Primitive(PrimitiveType {
                                        kind: PrimitiveKind::Float as i32,
                                    })),
                                    ..Default::default()
                                })),
                                ..Default::default()
                            })),
                            fixed_length: 1536,
                        }))),
                        ..Default::default()
                    })),
                    ..Default::default()
                }))
                .collect(),
            ..Default::default()
        }],
        ..Default::default()
    };

    let decoded = parse_schema_bundle(&bundle.encode_to_vec()).expect("supported refined bundle");
    let fields = &decoded.datasets[0].fields;
    assert!(matches!(
        fields[0]
            .r#type
            .as_deref()
            .and_then(|value| value.kind.as_ref()),
        Some(data_type::Kind::Decimal(DecimalType {
            precision: 18,
            scale: 4
        }))
    ));
    assert!(matches!(
        fields[1]
            .r#type
            .as_deref()
            .and_then(|value| value.kind.as_ref()),
        Some(data_type::Kind::Uuid(_))
    ));
    assert!(matches!(
        fields[2]
            .r#type
            .as_deref()
            .and_then(|value| value.kind.as_ref()),
        Some(data_type::Kind::FixedBytes(FixedBytesType {
            byte_length: 32
        }))
    ));
    let Some(data_type::Kind::List(list)) = fields[3]
        .r#type
        .as_deref()
        .and_then(|value| value.kind.as_ref())
    else {
        panic!("embedding must retain its fixed-list shape");
    };
    assert_eq!(list.fixed_length, 1536);
    assert!(matches!(
        list.element
            .as_deref()
            .and_then(|field| field.r#type.as_deref())
            .and_then(|value| value.kind.as_ref()),
        Some(data_type::Kind::Primitive(PrimitiveType { kind }))
            if *kind == PrimitiveKind::Float as i32
    ));
}

fn dataset<'a>(bundle: &'a SchemaBundle, source_message: &str) -> &'a invariant::DatasetSchema {
    bundle
        .datasets
        .iter()
        .find(|dataset| dataset.source_message == source_message)
        .expect("dataset exists")
}

fn field<'a>(fields: &'a [Field], name: &str) -> &'a Field {
    fields
        .iter()
        .find(|field| field.name == name)
        .expect("field exists")
}
