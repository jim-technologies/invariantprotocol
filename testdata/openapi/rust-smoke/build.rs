use prost::Message;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let descriptor = std::env::var_os("INVARIANT_OPENAPI_DESCRIPTOR")
        .ok_or("INVARIANT_OPENAPI_DESCRIPTOR is required")?;
    let image = std::fs::read(descriptor)?;
    let files = prost_types::FileDescriptorSet::decode(image.as_slice())?;
    invariant_protocol_codegen::configure().compile_fds(files)?;
    Ok(())
}
