//! CLI request-file, status, and middleware behavior.

mod common;

use common::{TestGreetService, greet, registered_server};
use invariant::{Code, ErasedRequest, Response, Status};
use prost::Message;
use serde_json::Value;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use tokio::io::AsyncReadExt;

async fn run_cli(server: Arc<invariant::Server>, args: &[&str]) -> Result<String, Status> {
    let (mut reader, mut writer) = tokio::io::duplex(64 * 1024);
    let args = args
        .iter()
        .map(|value| (*value).to_string())
        .collect::<Vec<_>>();
    let result = invariant::projections::cli::cli_write(server, &args, &mut writer).await;
    drop(writer);
    let mut output = String::new();
    reader.read_to_string(&mut output).await.unwrap();
    result.map(|()| output)
}

fn request_path(extension: &str) -> PathBuf {
    static NEXT: AtomicUsize = AtomicUsize::new(0);
    std::env::temp_dir().join(format!(
        "invariant-cli-{}-{}.{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed),
        extension
    ))
}

fn write_request(extension: &str, bytes: &[u8]) -> PathBuf {
    let path = request_path(extension);
    std::fs::write(&path, bytes).unwrap();
    path
}

fn remove_request(path: &Path) {
    let _ = std::fs::remove_file(path);
}

#[tokio::test]
async fn json_and_binary_request_files_use_canonical_decoding() {
    let json_path = write_request("json", br#"{"name":"JsonFile"}"#);
    let output = run_cli(
        registered_server(TestGreetService::default()),
        &["GreetService", "Greet", "-r", json_path.to_str().unwrap()],
    )
    .await
    .unwrap();
    remove_request(&json_path);
    assert_eq!(
        serde_json::from_str::<Value>(&output).unwrap()["message"],
        "Hi JsonFile"
    );

    let mut request = greet::GreetRequest {
        name: "BinaryFile".into(),
        ..Default::default()
    }
    .encode_to_vec();
    request.extend_from_slice(&[0x9a, 0x06, 0x03, b'n', b'e', b'w']);

    for extension in ["binpb", "pb"] {
        let path = write_request(extension, &request);
        let output = run_cli(
            registered_server(TestGreetService::default()),
            &["GreetService", "Greet", "-r", path.to_str().unwrap()],
        )
        .await
        .unwrap();
        remove_request(&path);
        let response: Value = serde_json::from_str(&output).unwrap();
        assert_eq!(response["message"], "Hi BinaryFile");
    }
}

#[tokio::test]
async fn request_parsing_is_strict_and_reports_invalid_argument() {
    let server = registered_server(TestGreetService::default());
    for args in [
        vec!["GreetService", "Greet", "extra"],
        vec!["GreetService", "Greet", "-r", "{}", "extra"],
        vec!["GreetService", "Greet", "-r", "{}", "-r", "{}"],
        vec![
            "GreetService",
            "Greet",
            "-r",
            r#"{"name":"Ada","extra":"x"}"#,
        ],
    ] {
        assert_eq!(
            run_cli(server.clone(), &args).await.unwrap_err().code(),
            Code::InvalidArgument
        );
    }

    for (extension, bytes) in [
        ("yaml", b"name: Ada".as_slice()),
        ("binpb", b"\xff".as_slice()),
        ("", br#"{"name":"Ada"}"#.as_slice()),
    ] {
        let path = write_request(extension, bytes);
        let status = run_cli(
            server.clone(),
            &["GreetService", "Greet", "-r", path.to_str().unwrap()],
        )
        .await
        .unwrap_err();
        remove_request(&path);
        assert_eq!(status.code(), Code::InvalidArgument);
    }

    let directory = request_path("json");
    std::fs::create_dir(&directory).unwrap();
    let status = run_cli(
        server,
        &["GreetService", "Greet", "-r", directory.to_str().unwrap()],
    )
    .await
    .unwrap_err();
    std::fs::remove_dir(&directory).unwrap();
    assert_eq!(status.code(), Code::InvalidArgument);
}

#[tokio::test]
async fn unary_cli_uses_the_typed_shared_chain_and_preserves_status() {
    let service = TestGreetService::default().with_greet(|request| async move {
        if request.get_ref().name == "status" {
            return Err(Status::failed_precondition("cli status"));
        }
        Ok(Response::new(greet::GreetResponse {
            message: format!("Hello {}", request.get_ref().name),
            ..Default::default()
        }))
    });
    let server = registered_server(service);
    let calls = Arc::new(AtomicUsize::new(0));
    let seen = calls.clone();
    server
        .use_shared_unary(Arc::new(move |request: ErasedRequest, info, next| {
            let seen = seen.clone();
            Box::pin(async move {
                seen.fetch_add(1, Ordering::SeqCst);
                assert_eq!(info.full_method, "/greet.v1.GreetService/Greet");
                assert!(request.downcast_ref::<greet::GreetRequest>().is_some());
                next(request).await
            })
        }))
        .unwrap();

    let output = run_cli(
        server.clone(),
        &["GreetService", "Greet", "-r", r#"{"name":"Typed"}"#],
    )
    .await
    .unwrap();
    assert_eq!(
        serde_json::from_str::<Value>(&output).unwrap()["message"],
        "Hello Typed"
    );

    let status = run_cli(
        server,
        &["GreetService", "Greet", "-r", r#"{"name":"status"}"#],
    )
    .await
    .unwrap_err();
    assert_eq!(status.code(), Code::FailedPrecondition);
    assert_eq!(status.message(), "cli status");
    assert_eq!(calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn streaming_cli_uses_the_typed_shared_chain_and_ndjson() {
    let server = registered_server(TestGreetService::default());
    let calls = Arc::new(AtomicUsize::new(0));
    let seen = calls.clone();
    server
        .use_shared_stream(Arc::new(move |request: ErasedRequest, info, next| {
            let seen = seen.clone();
            Box::pin(async move {
                seen.fetch_add(1, Ordering::SeqCst);
                assert_eq!(info.full_method, "/greet.v1.GreetService/StreamGreet");
                assert!(
                    request
                        .downcast_ref::<greet::StreamGreetRequest>()
                        .is_some()
                );
                next(request).await
            })
        }))
        .unwrap();

    let output = run_cli(
        server,
        &[
            "GreetService",
            "StreamGreet",
            "-r",
            r#"{"name":"Stream","count":2}"#,
        ],
    )
    .await
    .unwrap();
    let messages = output
        .lines()
        .map(|line| serde_json::from_str::<Value>(line).unwrap()["message"].clone())
        .collect::<Vec<_>>();
    assert_eq!(messages, ["Hi Stream #0", "Hi Stream #1"]);
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}
