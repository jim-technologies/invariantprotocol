//! Generate Rust bindings from the exact descriptor images used at runtime.

use prost::Message;
use prost_types::{
    DescriptorProto, FieldDescriptorProto, FileDescriptorProto, FileDescriptorSet,
    MethodDescriptorProto, ServiceDescriptorProto, field_descriptor_proto,
};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("..");
    let service_image = root.join("python/tests/proto/descriptor.binpb");
    let data_image = root.join("proto/descriptor.binpb");
    println!("cargo:rerun-if-changed={}", service_image.display());
    println!("cargo:rerun-if-changed={}", data_image.display());

    let service_fds = FileDescriptorSet::decode(std::fs::read(service_image)?.as_slice())?;
    invariant_protocol_codegen::configure().compile_fds(service_fds)?;

    let data_fds = FileDescriptorSet::decode(std::fs::read(data_image)?.as_slice())?;
    invariant_protocol_codegen::configure()
        .runtime_path("::invariant")
        .compile_fds(data_fds)?;

    let cardinality_fds = cardinality_test_descriptor();
    std::fs::write(
        std::path::PathBuf::from(std::env::var_os("OUT_DIR").expect("OUT_DIR"))
            .join("cardinality.binpb"),
        cardinality_fds.encode_to_vec(),
    )?;
    invariant_protocol_codegen::configure().compile_fds(cardinality_fds)?;
    Ok(())
}

fn cardinality_test_descriptor() -> FileDescriptorSet {
    let message = |name: &str| DescriptorProto {
        name: Some(name.to_string()),
        field: vec![FieldDescriptorProto {
            name: Some("value".into()),
            number: Some(1),
            label: Some(field_descriptor_proto::Label::Optional as i32),
            r#type: Some(field_descriptor_proto::Type::String as i32),
            json_name: Some("value".into()),
            ..Default::default()
        }],
        ..Default::default()
    };
    let method = |name: &str, client_streaming, server_streaming| MethodDescriptorProto {
        name: Some(name.to_string()),
        input_type: Some(".cardinality.v1.Input".into()),
        output_type: Some(".cardinality.v1.Output".into()),
        client_streaming: Some(client_streaming),
        server_streaming: Some(server_streaming),
        ..Default::default()
    };
    FileDescriptorSet {
        file: vec![FileDescriptorProto {
            name: Some("cardinality/v1/cardinality.proto".into()),
            package: Some("cardinality.v1".into()),
            syntax: Some("proto3".into()),
            message_type: vec![message("Input"), message("Output")],
            service: vec![ServiceDescriptorProto {
                name: Some("AllCardinalityService".into()),
                method: vec![
                    method("Unary", false, false),
                    method("ServerStream", false, true),
                    method("ClientStream", true, false),
                    method("Bidi", true, true),
                ],
                ..Default::default()
            }],
            ..Default::default()
        }],
    }
}
