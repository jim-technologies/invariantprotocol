//! Generate Rust bindings from the exact descriptor images used at runtime.

use prost::Message;
use prost_types::FileDescriptorSet;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("..");
    let data_image = root.join("proto/descriptor.binpb");
    println!("cargo:rerun-if-changed={}", data_image.display());

    let data_fds = FileDescriptorSet::decode(std::fs::read(data_image)?.as_slice())?;
    invariant_protocol_codegen::configure()
        .runtime_path("::invariant")
        .compile_fds(data_fds)?;

    // Cargo exposes this marker while compiling the primary package's build
    // script, so repository tests keep their fixtures without generating them
    // when Invariant is built as a dependency.
    if option_env!("CARGO_PRIMARY_PACKAGE").is_some() {
        let service_image = root.join("python/tests/proto/descriptor.binpb");
        println!("cargo:rerun-if-changed={}", service_image.display());
        let service_fds = FileDescriptorSet::decode(std::fs::read(service_image)?.as_slice())?;
        invariant_protocol_codegen::configure().compile_fds(service_fds)?;

        let cardinality_image = root.join("conformance/proto/descriptor.binpb");
        println!("cargo:rerun-if-changed={}", cardinality_image.display());
        let cardinality_bytes = std::fs::read(cardinality_image)?;
        let cardinality_fds = FileDescriptorSet::decode(cardinality_bytes.as_slice())?;
        std::fs::write(
            std::path::PathBuf::from(std::env::var_os("OUT_DIR").expect("OUT_DIR"))
                .join("cardinality.binpb"),
            cardinality_bytes,
        )?;
        invariant_protocol_codegen::configure().compile_fds(cardinality_fds)?;
    }
    Ok(())
}
