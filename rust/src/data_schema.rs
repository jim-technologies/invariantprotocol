//! Validated access to the generated, language-neutral data-schema bundle.

use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage, Value};
use std::sync::LazyLock;
use thiserror::Error;

use crate::data::v1::{Field, data_type};
use crate::{DatasetSchema, SchemaBundle};

pub const SCHEMA_IR_VERSION: u32 = 4;
pub const SCHEMA_MAPPING_VERSION: u32 = 3;

static SCHEMA_DESCRIPTOR_POOL: LazyLock<DescriptorPool> = LazyLock::new(|| {
    DescriptorPool::decode(include_bytes!("../../proto/descriptor.binpb").as_ref())
        .expect("the embedded Invariant descriptor image is valid")
});

#[derive(Debug, Error)]
pub enum DataSchemaError {
    #[error("decode SchemaBundle: {0}")]
    Decode(#[from] prost::DecodeError),
    #[error("unsupported SchemaBundle ir_version {found}; expected {expected}")]
    UnsupportedIrVersion { found: u32, expected: u32 },
    #[error("unsupported SchemaBundle mapping_version {found}; expected {expected}")]
    UnsupportedMappingVersion { found: u32, expected: u32 },
    #[error(
        "unsupported SchemaBundle version pair ir_version={ir_version} mapping_version={mapping_version}; expected 3/2 or {expected_ir}/{expected_mapping}"
    )]
    UnsupportedVersionPair {
        ir_version: u32,
        mapping_version: u32,
        expected_ir: u32,
        expected_mapping: u32,
    },
    #[error(
        "SchemaBundle mapping_version 2 field {path:?} contains fixed_length {length}, which was introduced in mapping_version 3"
    )]
    LegacyFixedLength { path: String, length: u32 },
    #[error("migrate SchemaBundle: legacy artifact contains fields unknown to this migrator")]
    LegacyUnknownFields,
}

/// Decode a bundle and reject IR or mapping rules this package cannot interpret.
pub fn parse_schema_bundle(data: &[u8]) -> Result<SchemaBundle, DataSchemaError> {
    let bundle = SchemaBundle::decode(data)?;
    if (bundle.ir_version, bundle.mapping_version) == (3, 2) {
        let descriptor = SCHEMA_DESCRIPTOR_POOL
            .get_message_by_name("invariant.data.v1.SchemaBundle")
            .expect("the embedded descriptor image contains SchemaBundle");
        let dynamic = DynamicMessage::decode(descriptor, data)?;
        if contains_unknown_fields(&dynamic) {
            return Err(DataSchemaError::LegacyUnknownFields);
        }
    }
    migrate_schema_bundle(bundle)
}

/// Upgrade the one supported historical SchemaBundle version in memory.
///
/// Serialized artifacts must enter through [`parse_schema_bundle`] so unknown
/// legacy wire fields are rejected before prost decodes into this owned value.
pub fn migrate_schema_bundle(mut bundle: SchemaBundle) -> Result<SchemaBundle, DataSchemaError> {
    match (bundle.ir_version, bundle.mapping_version) {
        (SCHEMA_IR_VERSION, SCHEMA_MAPPING_VERSION) => Ok(bundle),
        (3, 2) => {
            for dataset in &bundle.datasets {
                validate_legacy_fields(&dataset.fields, &dataset.name)?;
            }
            bundle.ir_version = SCHEMA_IR_VERSION;
            bundle.mapping_version = SCHEMA_MAPPING_VERSION;
            Ok(bundle)
        }
        (ir_version, mapping_version) => Err(DataSchemaError::UnsupportedVersionPair {
            ir_version,
            mapping_version,
            expected_ir: SCHEMA_IR_VERSION,
            expected_mapping: SCHEMA_MAPPING_VERSION,
        }),
    }
}

fn contains_unknown_fields(message: &DynamicMessage) -> bool {
    message.unknown_fields().next().is_some()
        || message
            .fields()
            .any(|(_, value)| value_contains_unknown_fields(value))
}

fn value_contains_unknown_fields(value: &Value) -> bool {
    match value {
        Value::Message(message) => contains_unknown_fields(message),
        Value::List(values) => values.iter().any(value_contains_unknown_fields),
        Value::Map(values) => values.values().any(value_contains_unknown_fields),
        _ => false,
    }
}

/// Reject IR or mapping rules this package cannot interpret.
pub fn validate_schema_bundle(bundle: &SchemaBundle) -> Result<(), DataSchemaError> {
    if bundle.ir_version != SCHEMA_IR_VERSION {
        return Err(DataSchemaError::UnsupportedIrVersion {
            found: bundle.ir_version,
            expected: SCHEMA_IR_VERSION,
        });
    }
    if bundle.mapping_version != SCHEMA_MAPPING_VERSION {
        return Err(DataSchemaError::UnsupportedMappingVersion {
            found: bundle.mapping_version,
            expected: SCHEMA_MAPPING_VERSION,
        });
    }
    Ok(())
}

fn validate_legacy_fields(fields: &[Field], parent: &str) -> Result<(), DataSchemaError> {
    for field in fields {
        let path = if parent.is_empty() {
            field.name.clone()
        } else {
            format!("{parent}.{}", field.name)
        };
        let Some(data_type) = field.r#type.as_deref() else {
            continue;
        };
        match data_type.kind.as_ref() {
            Some(data_type::Kind::Struct(value)) => {
                validate_legacy_fields(&value.fields, &path)?;
            }
            Some(data_type::Kind::List(value)) => {
                if value.fixed_length != 0 {
                    return Err(DataSchemaError::LegacyFixedLength {
                        path,
                        length: value.fixed_length,
                    });
                }
                if let Some(element) = value.element.as_deref() {
                    validate_legacy_fields(std::slice::from_ref(element), &format!("{path}[]"))?;
                }
            }
            Some(data_type::Kind::Map(value)) => {
                if let Some(key) = value.key.as_deref() {
                    validate_legacy_fields(std::slice::from_ref(key), &format!("{path}.key"))?;
                }
                if let Some(item) = value.value.as_deref() {
                    validate_legacy_fields(std::slice::from_ref(item), &format!("{path}.value"))?;
                }
            }
            _ => {}
        }
    }
    Ok(())
}

/// Find a dataset by its fully-qualified protobuf source message name.
pub fn find_dataset<'a>(
    bundle: &'a SchemaBundle,
    source_message: &str,
) -> Option<&'a DatasetSchema> {
    bundle
        .datasets
        .iter()
        .find(|dataset| dataset.source_message == source_message)
}
