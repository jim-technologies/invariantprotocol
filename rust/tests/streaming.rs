//! Server-streaming projection behavior from a generated tonic service.

mod common;

use common::{TestGreetService, greet, registered_server};
use futures::StreamExt;
use invariant::{BoxResponseStream, MethodConfig, Request, Response, Status};
use prost::Message;
use prost_reflect::DynamicMessage;
use serde_json::json;
use std::sync::Arc;
use tokio::io::AsyncReadExt;

fn stream_with_error() -> TestGreetService {
    TestGreetService::default().with_stream(|request| async move {
        let name = request.into_inner().name;
        let stream = async_stream::stream! {
            yield Ok(greet::GreetResponse {
                message: format!("Hi {name} #0"),
                ..Default::default()
            });
            let mut metadata = tonic::metadata::MetadataMap::new();
            metadata.insert("x-stream-error", "stream-7".parse().unwrap());
            yield Err(Status::with_metadata(
                tonic::Code::FailedPrecondition,
                "stream stopped",
                metadata,
            ));
        };
        Ok(Response::new(Box::pin(stream) as BoxResponseStream<_>))
    })
}

fn dynamic_stream_request(server: &invariant::Server, name: &str, count: i32) -> DynamicMessage {
    let descriptor = server
        .parsed()
        .pool
        .get_message_by_name("greet.v1.StreamGreetRequest")
        .unwrap();
    DynamicMessage::decode(
        descriptor,
        greet::StreamGreetRequest {
            name: name.into(),
            count,
        }
        .encode_to_vec()
        .as_slice(),
    )
    .unwrap()
}

#[tokio::test]
async fn programmatic_streaming_projects_the_registered_typed_service() {
    let server = registered_server(TestGreetService::default());
    let request = dynamic_stream_request(&server, "Programmatic", 3);
    let mut stream = server
        .invoke_stream("GreetService.StreamGreet", Request::new(request))
        .await
        .unwrap()
        .into_inner();
    let mut messages = Vec::new();
    while let Some(message) = stream.next().await {
        let message =
            greet::GreetResponse::decode(message.unwrap().encode_to_vec().as_slice()).unwrap();
        messages.push(message.message);
    }
    assert_eq!(
        messages,
        [
            "Hi Programmatic #0",
            "Hi Programmatic #1",
            "Hi Programmatic #2"
        ]
    );
}

async fn start_http(server: Arc<invariant::Server>) -> (String, tokio::task::JoinHandle<()>) {
    let app = invariant::projections::http::http_router(server);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{address}"), task)
}

fn envelope(flags: u8, payload: &[u8]) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(payload.len() + 5);
    bytes.push(flags);
    bytes.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    bytes.extend_from_slice(payload);
    bytes
}

fn unpack_all(mut bytes: &[u8]) -> Vec<(u8, Vec<u8>)> {
    let mut envelopes = Vec::new();
    while !bytes.is_empty() {
        assert!(bytes.len() >= 5);
        let flags = bytes[0];
        let len = u32::from_be_bytes(bytes[1..5].try_into().unwrap()) as usize;
        assert!(bytes.len() >= 5 + len);
        envelopes.push((flags, bytes[5..5 + len].to_vec()));
        bytes = &bytes[5 + len..];
    }
    envelopes
}

#[tokio::test]
async fn connect_streaming_emits_messages_and_a_standard_error_end_envelope() {
    let (url, task) = start_http(registered_server(stream_with_error())).await;
    let request = serde_json::to_vec(&json!({"name": "Connect", "count": 2})).unwrap();
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", "application/connect+json")
        .body(envelope(0, &request))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(
        response.headers()["content-type"],
        "application/connect+json"
    );
    let envelopes = unpack_all(&response.bytes().await.unwrap());
    assert_eq!(envelopes.len(), 2);
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&envelopes[0].1).unwrap()["message"],
        "Hi Connect #0"
    );
    assert_eq!(envelopes[1].0, 0x02);
    let end: serde_json::Value = serde_json::from_slice(&envelopes[1].1).unwrap();
    assert_eq!(end["error"]["code"], "failed_precondition");
    assert_eq!(end["error"]["message"], "stream stopped");
    assert_eq!(end["metadata"]["x-stream-error"][0], "stream-7");
    task.abort();
}

#[tokio::test]
async fn connect_stream_request_limit_is_per_message_and_preallocation_safe() {
    let server = registered_server(TestGreetService::default());
    server.set_max_stream_request_bytes(16).unwrap();
    let (url, task) = start_http(server).await;
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", "application/connect+proto")
        .body(envelope(0, &[0; 32]))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "resource_exhausted"
    );
    task.abort();
}

#[tokio::test]
async fn connect_stream_response_limit_is_per_message_with_method_override() {
    let server = registered_server(TestGreetService::default());
    server.set_max_stream_request_bytes(8).unwrap();
    server.set_max_stream_response_bytes(8).unwrap();
    server
        .configure_method(
            "/greet.v1.GreetService/StreamGreet",
            MethodConfig {
                max_stream_request_bytes: 128,
                max_stream_response_bytes: 64,
                ..Default::default()
            },
        )
        .unwrap();
    let (url, task) = start_http(server).await;
    let request = serde_json::to_vec(&json!({"name": "bounded", "count": 4})).unwrap();
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", "application/connect+json")
        .body(envelope(0, &request))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let envelopes = unpack_all(&response.bytes().await.unwrap());
    assert_eq!(envelopes.len(), 5);
    assert!(envelopes[..4].iter().all(|(flags, _)| *flags == 0));
    assert_eq!(envelopes[4].0, 0x02);
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&envelopes[4].1).unwrap(),
        json!({})
    );
    assert!(
        envelopes[..4]
            .iter()
            .map(|(_, payload)| payload.len())
            .sum::<usize>()
            > 64
    );
    task.abort();

    let service = TestGreetService::default().with_stream(|_| async {
        let stream = futures::stream::iter([Ok(greet::GreetResponse {
            message: "x".repeat(128),
            ..Default::default()
        })]);
        Ok(Response::new(Box::pin(stream) as BoxResponseStream<_>))
    });
    let server = registered_server(service);
    server.set_max_stream_response_bytes(64).unwrap();
    let (url, task) = start_http(server).await;
    let request = serde_json::to_vec(&json!({"name": "large"})).unwrap();
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", "application/connect+json")
        .body(envelope(0, &request))
        .send()
        .await
        .unwrap();
    let envelopes = unpack_all(&response.bytes().await.unwrap());
    assert_eq!(envelopes.len(), 1);
    let end: serde_json::Value = serde_json::from_slice(&envelopes[0].1).unwrap();
    assert_eq!(end["error"]["code"], "resource_exhausted");
    task.abort();
}

#[tokio::test]
async fn connect_stream_deadline_is_reported_in_the_end_envelope() {
    let service = TestGreetService::default().with_stream(|_| async {
        let stream = futures::stream::pending();
        Ok(Response::new(Box::pin(stream) as BoxResponseStream<_>))
    });
    let (url, task) = start_http(registered_server(service)).await;
    let request = serde_json::to_vec(&json!({"name": "slow"})).unwrap();
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", "application/connect+json")
        .header("connect-timeout-ms", "20")
        .body(envelope(0, &request))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let envelopes = unpack_all(&response.bytes().await.unwrap());
    let end: serde_json::Value = serde_json::from_slice(&envelopes.last().unwrap().1).unwrap();
    assert_eq!(end["error"]["code"], "deadline_exceeded");
    task.abort();
}

#[tokio::test]
async fn mcp_and_cli_stream_the_same_registered_implementation() {
    let server = registered_server(TestGreetService::default());
    let response = invariant::projections::mcp::mcp_dispatch(
        &server,
        &json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": {
                "name": "GreetService.StreamGreet",
                "arguments": {"name": "MCP", "count": 2}
            }
        }),
    )
    .await
    .unwrap();
    let content = response["result"]["content"].as_array().unwrap();
    assert_eq!(content.len(), 2);
    assert!(content[0]["text"].as_str().unwrap().contains("Hi MCP #0"));

    let (mut reader, mut writer) = tokio::io::duplex(4096);
    let cli_server = server.clone();
    let task = tokio::spawn(async move {
        invariant::projections::cli::cli_write(
            cli_server,
            &[
                "GreetService".into(),
                "StreamGreet".into(),
                "-r".into(),
                r#"{"name":"CLI","count":2}"#.into(),
            ],
            &mut writer,
        )
        .await
        .unwrap();
    });
    let mut output = String::new();
    reader.read_to_string(&mut output).await.unwrap();
    task.await.unwrap();
    assert_eq!(output.lines().count(), 2);
    assert!(output.contains("Hi CLI #0"));
    assert!(output.contains("Hi CLI #1"));
}

#[tokio::test]
async fn mcp_stream_error_keeps_prior_chunks_and_standard_status() {
    let server = registered_server(stream_with_error());
    let response = invariant::projections::mcp::mcp_dispatch(
        &server,
        &json!({
            "jsonrpc": "2.0",
            "id": "stream-error",
            "method": "tools/call",
            "params": {
                "name": "GreetService.StreamGreet",
                "arguments": {"name": "MCP"}
            }
        }),
    )
    .await
    .unwrap();
    assert_eq!(response["result"]["isError"], true);
    assert_eq!(response["result"]["error"]["code"], "failed_precondition");
    assert_eq!(response["result"]["content"].as_array().unwrap().len(), 2);
}
