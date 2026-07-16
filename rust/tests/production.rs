//! Failure isolation, trust boundaries, and projection lifecycle checks.

mod common;

use common::{TestGreetService, greet, registered_server, serve_native};
use invariant::{BoxResponseStream, Code, Request, Response, Status};
use prost::Message;
use prost_reflect::DynamicMessage;
use serde_json::json;
use std::sync::Arc;

fn dynamic_greet_request(server: &invariant::Server) -> DynamicMessage {
    let descriptor = server
        .parsed()
        .pool
        .get_message_by_name("greet.v1.GreetRequest")
        .unwrap();
    DynamicMessage::decode(
        descriptor,
        greet::GreetRequest {
            name: "panic".into(),
            ..Default::default()
        }
        .encode_to_vec()
        .as_slice(),
    )
    .unwrap()
}

#[tokio::test]
async fn handler_panics_become_internal_status_without_crashing_the_process() {
    let service = TestGreetService::default().with_greet(|_| async {
        panic!("kaboom");
        #[allow(unreachable_code)]
        Ok(Response::new(greet::GreetResponse::default()))
    });
    let server = registered_server(service);
    let status = server
        .invoke(
            "GreetService.Greet",
            Request::new(dynamic_greet_request(&server)),
        )
        .await
        .unwrap_err();
    assert_eq!(status.code(), Code::Internal);
    assert!(status.message().contains("kaboom"));
    assert!(status.message().contains("/greet.v1.GreetService/Greet"));
}

#[tokio::test]
async fn stream_setup_panics_become_internal_status() {
    let service = TestGreetService::default().with_stream(|_| async {
        panic!("stream-kaboom");
        #[allow(unreachable_code)]
        Ok(Response::new(
            Box::pin(futures::stream::empty()) as BoxResponseStream<_>
        ))
    });
    let server = registered_server(service);
    let descriptor = server
        .parsed()
        .pool
        .get_message_by_name("greet.v1.StreamGreetRequest")
        .unwrap();
    let status = match server
        .invoke_stream(
            "GreetService.StreamGreet",
            Request::new(DynamicMessage::new(descriptor)),
        )
        .await
    {
        Ok(_) => panic!("panicking stream unexpectedly started"),
        Err(status) => status,
    };
    assert_eq!(status.code(), Code::Internal);
    assert!(status.message().contains("stream-kaboom"));
    assert!(
        status
            .message()
            .contains("/greet.v1.GreetService/StreamGreet")
    );
}

#[tokio::test]
async fn native_midstream_status_is_delivered_in_grpc_trailers() {
    let service = TestGreetService::default().with_stream(|_| async {
        let stream = async_stream::stream! {
            yield Ok(greet::GreetResponse {
                message: "first".into(),
                ..Default::default()
            });
            yield Err(Status::failed_precondition("kapow"));
        };
        Ok(Response::new(Box::pin(stream) as BoxResponseStream<_>))
    });
    let server = registered_server(service);
    let (address, task) = serve_native(server).await;
    let mut client = common::generated_client(address).await;
    let mut stream = client
        .stream_greet(greet::StreamGreetRequest::default())
        .await
        .unwrap()
        .into_inner();
    assert_eq!(stream.message().await.unwrap().unwrap().message, "first");
    let status = stream.message().await.unwrap_err();
    assert_eq!(status.code(), Code::FailedPrecondition);
    assert_eq!(status.message(), "kapow");
    task.abort();
}

#[tokio::test]
async fn untrusted_http_headers_do_not_become_grpc_metadata() {
    let service = TestGreetService::default().with_greet(|request| async move {
        for forbidden in ["authorization", "tenant", "principal", "role", "user"] {
            assert!(request.metadata().get(forbidden).is_none());
        }
        Ok(Response::new(greet::GreetResponse {
            message: "safe".into(),
            ..Default::default()
        }))
    });
    let server = registered_server(service);
    let app = invariant::projections::http::http_router(server);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    let response = reqwest::Client::new()
        .post(format!("http://{address}/greet.v1.GreetService/Greet"))
        .header("authorization", "Bearer attacker")
        .header("tenant", "attacker")
        .header("principal", "admin")
        .header("role", "owner")
        .header("user", "root")
        .json(&json!({"name": "safe"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    task.abort();
}

#[tokio::test]
async fn mcp_notifications_have_no_response_and_unknown_methods_are_json_rpc_errors() {
    let server = registered_server(TestGreetService::default());
    let initialized = invariant::projections::mcp::mcp_dispatch(
        &server,
        &json!({"jsonrpc": "2.0", "id": 1, "method": "initialize"}),
    )
    .await
    .unwrap();
    assert_eq!(initialized["result"]["protocolVersion"], "2025-11-25");
    assert!(
        invariant::projections::mcp::mcp_dispatch(
            &server,
            &json!({"jsonrpc": "2.0", "method": "notifications/initialized"}),
        )
        .await
        .is_none()
    );
    let response = invariant::projections::mcp::mcp_dispatch(
        &server,
        &json!({"jsonrpc": "2.0", "id": 1, "method": "missing"}),
    )
    .await
    .unwrap();
    assert_eq!(response["error"]["code"], -32601);
}

#[tokio::test]
async fn projection_runner_honors_cancellation() {
    let server = registered_server(TestGreetService::default());
    let cancel = tokio_util::sync::CancellationToken::new();
    let trigger = cancel.clone();
    tokio::spawn(async move {
        tokio::task::yield_now().await;
        trigger.cancel();
    });
    tokio::time::timeout(
        std::time::Duration::from_secs(2),
        invariant::projections::serve::serve(
            server,
            [invariant::projections::serve::Projection::Http(0)],
            cancel,
        ),
    )
    .await
    .unwrap()
    .unwrap();
}

#[test]
fn server_handle_is_cloneable_without_duplicating_registration_state() {
    let server = registered_server(TestGreetService::default());
    let clone = Arc::clone(&server);
    assert_eq!(clone.tool_catalog().len(), server.tool_catalog().len());
}
