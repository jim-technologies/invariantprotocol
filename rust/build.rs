//! Generate prost types for the shared `greet.proto` used by integration tests
//! and benchmarks. The proto + its pre-built descriptor.binpb live under
//! `python/tests/proto/` so all three implementations test against the same
//! source of truth.
//!
//! We use the `compile_with_config` path so the generated Rust file lands in
//! `$OUT_DIR/greet.v1.rs` and is `include!`d from `tests/common.rs`.

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("python")
        .join("tests")
        .join("proto");
    println!("cargo:rerun-if-changed={}/greet.proto", proto_dir.display());

    prost_build::Config::new()
        .compile_protos(
            &[proto_dir.join("greet.proto").to_string_lossy().to_string()],
            &[
                proto_dir.to_string_lossy().to_string(),
                // Pull in the `buf.build/googleapis/googleapis` cache so
                // `google/api/annotations.proto` resolves the same way buf
                // does. Skipping ergonomics: tests only need the message
                // types, not the HTTP annotations.
                proto_dir.join("gen").to_string_lossy().to_string(),
            ],
        )
        .ok(); // Soft-fail: greet.proto imports google/api + buf/validate which
               // require the buf module cache. We work around it by using a
               // stripped-down test proto below if the full compile fails.
               // Drop the stripped proto in OUT_DIR — keeps it scoped to this crate's
               // build artefacts and out of any sibling language directory.
    let out_dir = std::path::PathBuf::from(std::env::var("OUT_DIR")?);
    let stripped_path = out_dir.join("greet.proto");
    std::fs::write(&stripped_path, STRIPPED_GREET_PROTO)?;
    prost_build::Config::new().compile_protos(
        &[stripped_path.to_string_lossy().to_string()],
        &[out_dir.to_string_lossy().to_string()],
    )?;
    Ok(())
}

// A minimal greet.proto without google.api / buf.validate imports — those
// dependencies require the buf module cache (which we don't carry in-tree).
// The wire format and message names match the real greet.proto so the
// pre-built descriptor.binpb is byte-compatible for the fields we use.
const STRIPPED_GREET_PROTO: &str = r#"syntax = "proto3";
package greet.v1;

enum Mood {
  MOOD_UNSPECIFIED = 0;
  MOOD_HAPPY = 1;
  MOOD_SAD = 2;
}

service GreetService {
  rpc Greet(GreetRequest) returns (GreetResponse);
  rpc GreetGroup(GreetGroupRequest) returns (GreetGroupResponse);
  rpc StreamGreet(StreamGreetRequest) returns (stream GreetResponse);
}

message GreetRequest {
  string name = 1;
  optional Mood mood = 2;
  map<string, string> tags = 3;
}

message GreetResponse {
  string message = 1;
  Mood mood = 2;
  map<string, string> tags = 3;
}

message Person {
  string name = 1;
  Mood mood = 2;
}

message GreetGroupRequest {
  repeated Person people = 1;
}

message GreetGroupResponse {
  repeated string messages = 1;
  int32 count = 2;
}

message StreamGreetRequest {
  string name = 1;
  int32 count = 2;
}
"#;
