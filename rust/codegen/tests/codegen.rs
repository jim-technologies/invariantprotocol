use invariant_protocol_codegen::configure;
use prost_types::{
    DescriptorProto, FileDescriptorProto, FileDescriptorSet, MethodDescriptorProto,
    ServiceDescriptorProto,
};
use std::fs;
use std::time::{SystemTime, UNIX_EPOCH};

#[test]
fn custom_builder_emits_tonic_and_invariant_surfaces_for_every_cardinality() {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock is before the Unix epoch")
        .as_nanos();
    let out_dir =
        std::env::temp_dir().join(format!("invariant-codegen-{}-{nonce}", std::process::id()));
    fs::create_dir_all(&out_dir).expect("create codegen output directory");

    let method = |name: &str, client_streaming, server_streaming| MethodDescriptorProto {
        name: Some(name.to_string()),
        input_type: Some(".audit.v1.Request".to_string()),
        output_type: Some(".audit.v1.Response".to_string()),
        client_streaming: Some(client_streaming),
        server_streaming: Some(server_streaming),
        ..Default::default()
    };
    let descriptor = FileDescriptorSet {
        file: vec![FileDescriptorProto {
            name: Some("audit.proto".to_string()),
            package: Some("audit.v1".to_string()),
            syntax: Some("proto3".to_string()),
            message_type: vec![
                DescriptorProto {
                    name: Some("Request".to_string()),
                    ..Default::default()
                },
                DescriptorProto {
                    name: Some("Response".to_string()),
                    ..Default::default()
                },
            ],
            service: vec![ServiceDescriptorProto {
                name: Some("AuditService".to_string()),
                method: vec![
                    method("Check", false, false),
                    method("Watch", false, true),
                    method("Upload", true, false),
                    method("Chat", true, true),
                ],
                ..Default::default()
            }],
            ..Default::default()
        }],
    };

    configure()
        .out_dir(&out_dir)
        .runtime_path("::renamed_invariant")
        .compile_fds(descriptor)
        .expect("generate services from the descriptor image");

    let generated =
        fs::read_to_string(out_dir.join("audit.v1.rs")).expect("read generated Rust source");
    assert!(generated.contains("pub mod audit_service_client"));
    assert!(generated.contains("pub mod audit_service_server"));
    assert!(generated.contains("pub fn register_audit_service_server<"));
    assert!(generated.contains("pub fn register_audit_service_server_with<"));
    assert!(generated.contains("server: ::renamed_invariant::Server"));
    assert!(generated.contains("::renamed_invariant::ServerCallInfo::new"));
    assert!(generated.contains("invoke_typed_unary"));
    assert!(generated.contains("invoke_typed_stream"));
    assert!(generated.contains("invoke_typed_stream_call"));
    assert_eq!(generated.matches(".unary::<").count(), 1);
    assert_eq!(generated.matches(".server_streaming::<").count(), 1);

    fs::remove_dir_all(out_dir).expect("remove codegen output directory");
}
