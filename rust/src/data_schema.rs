//! Validated access to the generated, language-neutral data-schema bundle.

use prost::Message;
use thiserror::Error;

use crate::{DatasetSchema, SchemaBundle};

pub const SCHEMA_IR_VERSION: u32 = 2;
pub const SCHEMA_MAPPING_VERSION: u32 = 2;

#[derive(Debug, Error)]
pub enum DataSchemaError {
    #[error("decode SchemaBundle: {0}")]
    Decode(#[from] prost::DecodeError),
    #[error("unsupported SchemaBundle ir_version {found}; expected {expected}")]
    UnsupportedIrVersion { found: u32, expected: u32 },
    #[error("unsupported SchemaBundle mapping_version {found}; expected {expected}")]
    UnsupportedMappingVersion { found: u32, expected: u32 },
}

/// Decode a bundle and reject IR or mapping rules this package cannot interpret.
pub fn parse_schema_bundle(data: &[u8]) -> Result<SchemaBundle, DataSchemaError> {
    let bundle = SchemaBundle::decode(data)?;
    validate_schema_bundle(&bundle)?;
    Ok(bundle)
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
