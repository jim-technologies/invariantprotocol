//! HTTP / Connect projection integration tests.
//!
//! Mirrors `go/http_test.go` / `python/tests/test_http.py` shape — we exercise
//! the same wire surface so the three implementations are interchangeable from
//! a client's perspective.

mod common;

use common::{DESCRIPTOR_PATH, greet};
use invariant::{Server, Status, projections::http::http_router};
use prost::Message;
use std::sync::Arc;

async fn greet_handler(req: greet::GreetRequest) -> Result<greet::GreetResponse, Status> {
    Ok(greet::GreetResponse {
        message: format!("Hi {}", req.name),
        ..Default::default()
    })
}

async fn echo_tags(req: greet::GreetRequest) -> Result<greet::GreetResponse, Status> {
    Ok(greet::GreetResponse {
        message: format!("Hi {}", req.name),
        mood: req.mood.unwrap_or(0),
        tags: req.tags,
    })
}

async fn start_server() -> (String, tokio::task::JoinHandle<()>) {
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).expect("descriptor");
    srv.register_unary("GreetService.Greet", greet_handler);
    srv.register_unary("GreetService.GreetGroup", greet_group_handler);
    let app = http_router(Arc::new(srv));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let url = format!("http://{addr}");
    let handle = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    // Give axum a moment to start accepting.
    tokio::task::yield_now().await;
    (url, handle)
}

async fn greet_group_handler(
    req: greet::GreetGroupRequest,
) -> Result<greet::GreetGroupResponse, Status> {
    let messages: Vec<String> = req
        .people
        .iter()
        .map(|p| format!("Hi {}", p.name))
        .collect();
    let count = messages.len() as i32;
    Ok(greet::GreetGroupResponse { messages, count })
}

#[tokio::test]
async fn http_unary_json_roundtrip() {
    let (url, handle) = start_server().await;
    let client = reqwest::Client::new();
    let resp = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&serde_json::json!({"name": "World"}))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["message"], "Hi World");
    handle.abort();
}

#[tokio::test]
async fn http_unary_proto_roundtrip() {
    let (url, handle) = start_server().await;
    let req = greet::GreetRequest {
        name: "Bin".into(),
        ..Default::default()
    };
    let body = req.encode_to_vec();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .header("content-type", "application/proto")
        .header("accept", "application/proto")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let bytes = resp.bytes().await.unwrap();
    let resp_msg = greet::GreetResponse::decode(&bytes[..]).unwrap();
    assert_eq!(resp_msg.message, "Hi Bin");
    handle.abort();
}

#[tokio::test]
async fn http_catalog_endpoint_lists_tools() {
    let (url, handle) = start_server().await;
    let body: serde_json::Value = reqwest::get(format!("{url}/"))
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let tools = body["tools"].as_array().unwrap();
    let names: Vec<&str> = tools.iter().map(|t| t["name"].as_str().unwrap()).collect();
    assert_eq!(names, vec!["GreetService.Greet", "GreetService.GreetGroup"]);
    handle.abort();
}

#[tokio::test]
async fn http_healthz_returns_ok() {
    let (url, handle) = start_server().await;
    for path in ["/healthz", "/readyz"] {
        let body: serde_json::Value = reqwest::get(format!("{url}{path}"))
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(body, serde_json::json!({"status": "ok"}));
    }
    handle.abort();
}

#[tokio::test]
async fn http_descriptor_endpoint_returns_fds() {
    let (url, handle) = start_server().await;
    let resp = reqwest::get(format!("{url}/__invariant/descriptor.binpb"))
        .await
        .unwrap();
    assert_eq!(resp.headers()["content-type"], "application/proto");
    let bytes = resp.bytes().await.unwrap();
    // Sanity: the bytes parse as a FileDescriptorSet.
    prost_types::FileDescriptorSet::decode(&bytes[..]).expect("valid FDS");
    handle.abort();
}

#[tokio::test]
async fn http_unknown_path_returns_404_envelope() {
    let (url, handle) = start_server().await;
    let resp = reqwest::Client::new()
        .post(format!("{url}/no.such.Service/Method"))
        .json(&serde_json::json!({}))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 404);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["code"], "not_found");
    handle.abort();
}

#[tokio::test]
async fn http_unary_unknown_field_surfaces_field_violation() {
    let (url, handle) = start_server().await;
    let resp = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&serde_json::json!({"name": "x", "extra": "y"}))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 400);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["code"], "invalid_argument");
    handle.abort();
}

#[tokio::test]
async fn http_unary_oversized_body_returns_resource_exhausted() {
    // Build a body well over the 16 MiB cap.
    let huge = "a".repeat(16 * 1024 * 1024 + 1024);
    let payload = serde_json::json!({"name": huge});
    let (url, handle) = start_server().await;
    let resp = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&payload)
        .send()
        .await
        .unwrap();
    assert!(resp.status().as_u16() >= 400);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["code"], "resource_exhausted");
    handle.abort();
}

#[tokio::test]
async fn http_connect_timeout_terminates_with_deadline_exceeded() {
    // Register a slow handler so the deadline fires.
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).expect("descriptor");
    srv.register_unary("GreetService.Greet", |_req: greet::GreetRequest| async {
        tokio::time::sleep(std::time::Duration::from_secs(60)).await;
        Ok::<_, Status>(greet::GreetResponse::default())
    });
    let app = http_router(Arc::new(srv));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    tokio::task::yield_now().await;

    let resp = reqwest::Client::new()
        .post(format!("http://{addr}/greet.v1.GreetService/Greet"))
        .header("connect-timeout-ms", "100")
        .json(&serde_json::json!({"name": "x"}))
        .send()
        .await
        .unwrap();
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["code"], "deadline_exceeded");
    handle.abort();
}

#[tokio::test]
async fn http_unary_preserves_enums_and_tags() {
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).expect("descriptor");
    srv.register_unary("GreetService.Greet", echo_tags);
    let app = http_router(Arc::new(srv));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    tokio::task::yield_now().await;

    let resp = reqwest::Client::new()
        .post(format!("http://{addr}/greet.v1.GreetService/Greet"))
        .json(&serde_json::json!({
            "name": "World",
            "mood": "MOOD_HAPPY",
            "tags": {"lang": "en"}
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["message"], "Hi World");
    assert_eq!(body["mood"], "MOOD_HAPPY");
    assert_eq!(body["tags"]["lang"], "en");
    handle.abort();
}
