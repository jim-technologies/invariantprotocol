//! Shared test fixtures: prost-generated greet types + descriptor path.

#![allow(clippy::all)]
#![allow(unused)]

pub mod greet {
    include!(concat!(env!("OUT_DIR"), "/greet.v1.rs"));
}

pub const DESCRIPTOR_PATH: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../python/tests/proto/descriptor.binpb"
);
