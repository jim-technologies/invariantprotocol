//! # invariant-protocol
//!
//! One proto descriptor in → Connect / gRPC / MCP / CLI projections out.
//!
//! Rust uses ordinary prost messages, tonic service traits, generated clients,
//! and generated service registration helpers. The descriptor image remains
//! the shared discovery/projection artifact and is checked against generated
//! code at registration time.

pub mod data_schema;
pub mod descriptor;
pub mod errors;
pub mod projections;
pub mod schema;
pub mod server;

/// Generated messages for the canonical CDC wire contract.
pub mod cdc {
    /// Version 1 of the transport-neutral change-record payload.
    pub mod v1 {
        include!(concat!(env!("OUT_DIR"), "/invariant.cdc.v1.rs"));
    }

    /// Version 2 with explicit full-image and replayable-delta representations.
    pub mod v2 {
        include!(concat!(env!("OUT_DIR"), "/invariant.cdc.v2.rs"));
    }
}

/// Generated messages for the stable CloudEvents protobuf envelope.
pub mod cloudevents {
    /// CloudEvents 1.0 protobuf format (`io.cloudevents.v1`).
    pub mod v1 {
        include!(concat!(env!("OUT_DIR"), "/io.cloudevents.v1.rs"));
    }
}

/// Generated messages for the language-neutral protobuf data contract.
pub mod data {
    /// Version 1 of the derived schema intermediate representation.
    pub mod v1 {
        include!(concat!(env!("OUT_DIR"), "/invariant.data.v1.rs"));
    }
}

pub use data::v1::{DatasetSchema, SchemaBundle};
pub use data_schema::{
    DataSchemaError, SCHEMA_IR_VERSION, SCHEMA_MAPPING_VERSION, find_dataset,
    migrate_schema_bundle, parse_schema_bundle, validate_schema_bundle,
};
pub use descriptor::{MethodInfo, ParsedDescriptor, ServiceInfo};
pub use server::{
    BoxResponseStream, DynamicResponseStream, ErasedRequest, ErasedResponse, HTTPMetadataMapper,
    MethodConfig, NativeService, ProjectionContext, Server, ServerCallInfo, ServiceRegistration,
    SharedHandler, SharedStreamMiddleware, SharedUnaryMiddleware, Tool,
    default_http_metadata_mapper,
};
pub use tonic::{Code, Request, Response, Status};

#[doc(hidden)]
pub use prost_reflect::{DescriptorPool, DynamicMessage, MessageDescriptor};
