use invariant::data::v1::{
    DataType, DatasetSchema, DecimalType, Field, FixedBytesType, Presence, SchemaBundle,
    SyntheticRole, UuidType, data_type,
};
use invariant::{DataSchemaError, parse_schema_bundle};
use prost::Message;

#[test]
fn reads_shared_canonical_schema_bundle() {
    let encoded = include_bytes!("../../testdata/data.schema.binpb");
    let bundle = parse_schema_bundle(encoded.as_slice()).expect("valid canonical schema bundle");

    assert_eq!(bundle.ir_version, 2);
    assert_eq!(bundle.mapping_version, 2);
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
        mapping_version: 2,
        ..Default::default()
    };
    assert!(matches!(
        invariant::validate_schema_bundle(&bundle),
        Err(DataSchemaError::UnsupportedIrVersion { .. })
    ));

    bundle.ir_version = 2;
    bundle.mapping_version = 1;
    assert!(matches!(
        invariant::validate_schema_bundle(&bundle),
        Err(DataSchemaError::UnsupportedMappingVersion { .. })
    ));
}

#[test]
fn round_trips_portable_refined_types() {
    let refined = [
        data_type::Kind::Decimal(DecimalType {
            precision: 18,
            scale: 4,
        }),
        data_type::Kind::Uuid(UuidType {}),
        data_type::Kind::FixedBytes(FixedBytesType { byte_length: 32 }),
    ];
    let bundle = SchemaBundle {
        ir_version: 2,
        mapping_version: 2,
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
