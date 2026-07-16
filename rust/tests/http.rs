//! Connect/HTTP projection tests over the same generated registration.

mod common;

use common::{TestGreetService, greet, registered_server};
use invariant::{
    Code, MethodConfig, ProjectionContext, Response, Status, projections::http::http_router,
};
use prost::Message;
use serde_json::json;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

async fn start_http(server: Arc<invariant::Server>) -> (String, tokio::task::JoinHandle<()>) {
    let app = http_router(server);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{address}"), task)
}

#[tokio::test]
async fn unary_json_and_proto_use_the_registered_generated_implementation() {
    let calls = Arc::new(AtomicUsize::new(0));
    let seen = calls.clone();
    let service = TestGreetService::default().with_greet(move |request| {
        let seen = seen.clone();
        async move {
            seen.fetch_add(1, Ordering::SeqCst);
            let request = request.into_inner();
            Ok(Response::new(greet::GreetResponse {
                message: format!("Projected {}", request.name),
                mood: request.mood.unwrap_or_default(),
                tags: request.tags,
            }))
        }
    });
    let server = registered_server(service);
    let (url, task) = start_http(server).await;
    let client = reqwest::Client::new();

    let response = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&serde_json::json!({
            "name": "JSON",
            "mood": "MOOD_HAPPY",
            "tags": {"format": "json"}
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let body: serde_json::Value = response.json().await.unwrap();
    assert_eq!(body["message"], "Projected JSON");
    assert_eq!(body["mood"], "MOOD_HAPPY");
    assert_eq!(body["tags"]["format"], "json");

    let response = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .header("content-type", "application/proto")
        .header("accept", "application/proto")
        .body(
            greet::GreetRequest {
                name: "Proto".into(),
                ..Default::default()
            }
            .encode_to_vec(),
        )
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let response = greet::GreetResponse::decode(response.bytes().await.unwrap()).unwrap();
    assert_eq!(response.message, "Projected Proto");
    assert_eq!(calls.load(Ordering::SeqCst), 2);
    task.abort();
}

#[tokio::test]
async fn catalog_descriptor_health_and_connect_error_shape_are_conventional() {
    let service = TestGreetService::default()
        .with_greet(|_| async { Err(Status::new(Code::Cancelled, "client left")) });
    let server = registered_server(service);
    let (url, task) = start_http(server).await;

    let catalog: serde_json::Value = reqwest::get(format!("{url}/"))
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    assert_eq!(catalog["tools"].as_array().unwrap().len(), 3);
    for path in ["/healthz", "/readyz"] {
        let body: serde_json::Value = reqwest::get(format!("{url}{path}"))
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(body, serde_json::json!({"status": "ok"}));
    }
    let descriptor = reqwest::get(format!("{url}/__invariant/descriptor.binpb"))
        .await
        .unwrap();
    assert_eq!(descriptor.headers()["content-type"], "application/proto");
    prost_types::FileDescriptorSet::decode(descriptor.bytes().await.unwrap()).unwrap();

    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&serde_json::json!({"name": "x"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 499);
    let body: serde_json::Value = response.json().await.unwrap();
    assert_eq!(body["code"], "canceled");
    assert_eq!(body["message"], "client left");
    task.abort();
}

#[tokio::test]
async fn unknown_json_field_has_a_connect_bad_request_detail() {
    let (url, task) = start_http(registered_server(TestGreetService::default())).await;
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&serde_json::json!({"name": "x", "invented": true}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 400);
    let body: serde_json::Value = response.json().await.unwrap();
    assert_eq!(body["code"], "invalid_argument");
    assert_eq!(
        body["details"][0]["type"], "google.rpc.BadRequest",
        "{body}"
    );
    assert!(
        body["details"][0]["value"]
            .as_str()
            .is_some_and(|value| !value.is_empty())
    );
    task.abort();
}

#[tokio::test]
async fn unary_request_limit_and_connect_timeout_are_enforced() {
    let cancelled = Arc::new(AtomicUsize::new(0));
    let observed = cancelled.clone();
    let service = TestGreetService::default().with_greet(move |request| {
        let observed = observed.clone();
        async move {
            let projection = request
                .extensions()
                .get::<ProjectionContext>()
                .unwrap()
                .clone();
            tokio::spawn(async move {
                projection.cancelled().await;
                observed.store(1, Ordering::SeqCst);
            });
            tokio::time::sleep(Duration::from_secs(60)).await;
            Ok(Response::new(greet::GreetResponse::default()))
        }
    });
    let server = registered_server(service);
    server.set_max_unary_request_bytes(64).unwrap();
    let (url, task) = start_http(server).await;
    let client = reqwest::Client::new();

    let response = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&serde_json::json!({"name": "x".repeat(128)}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "resource_exhausted"
    );

    let response = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .header("connect-timeout-ms", "20")
        .json(&serde_json::json!({"name": "slow"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 504);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "deadline_exceeded"
    );
    tokio::time::timeout(Duration::from_secs(1), async {
        while cancelled.load(Ordering::SeqCst) == 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    task.abort();
}

#[tokio::test]
async fn four_http_limits_are_independent_and_method_overrides_are_scoped() {
    let service = TestGreetService::default().with_greet(|request| async move {
        Ok(Response::new(greet::GreetResponse {
            message: request.into_inner().name.repeat(8),
            ..Default::default()
        }))
    });
    let server = registered_server(service);
    assert_eq!(server.max_unary_request_bytes(), 16 * 1024 * 1024);
    assert_eq!(server.max_unary_response_bytes(), 16 * 1024 * 1024);
    assert_eq!(server.max_stream_request_bytes(), 16 * 1024 * 1024);
    assert_eq!(server.max_stream_response_bytes(), 16 * 1024 * 1024);
    server.set_max_unary_request_bytes(64).unwrap();
    server.set_max_unary_response_bytes(64).unwrap();
    server
        .configure_method(
            "/greet.v1.GreetService/GreetGroup",
            MethodConfig {
                max_unary_request_bytes: 512,
                max_unary_response_bytes: 512,
                ..Default::default()
            },
        )
        .unwrap();
    let (url, task) = start_http(server).await;
    let client = reqwest::Client::new();

    let response = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&json!({"name": "x".repeat(128)}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "resource_exhausted"
    );

    let response = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&json!({"name": "small-response"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "resource_exhausted"
    );

    let response = client
        .post(format!("{url}/greet.v1.GreetService/GreetGroup"))
        .json(&json!({"people": [{"name": "y".repeat(96)}]}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["count"],
        1
    );
    task.abort();
}

#[tokio::test]
async fn http_metadata_is_filtered_and_response_metadata_and_context_are_tonic_shaped() {
    let service = TestGreetService::default().with_greet(|request| async move {
        assert_eq!(request.metadata().get("traceparent").unwrap(), "trace-1");
        assert_eq!(request.metadata().get("x-request-id").unwrap(), "request-2");
        assert_eq!(request.metadata().get("x-custom-safe").unwrap(), "safe-3");
        for denied in [
            "authorization",
            "x-tenant-id",
            "x-role",
            "invariant-internal-user",
        ] {
            assert!(!request.metadata().contains_key(denied));
        }
        let context = request.extensions().get::<ProjectionContext>().unwrap();
        assert!(context.deadline().is_some());
        assert!(!context.is_cancelled());
        assert!(request.metadata().contains_key("grpc-timeout"));

        if request.get_ref().name == "fail" {
            let mut metadata = tonic::metadata::MetadataMap::new();
            metadata.insert("x-error-id", "error-5".parse().unwrap());
            return Err(Status::with_metadata(
                Code::FailedPrecondition,
                "no",
                metadata,
            ));
        }
        let mut response = Response::new(greet::GreetResponse {
            message: "ok".into(),
            ..Default::default()
        });
        response
            .metadata_mut()
            .insert("x-response-id", "response-4".parse().unwrap());
        response
            .metadata_mut()
            .insert("authorization", "must-not-leak".parse().unwrap());
        Ok(response)
    });
    let server = registered_server(service);
    server
        .use_http_metadata_mapper(Arc::new(|headers| {
            let mut metadata = invariant::default_http_metadata_mapper(headers);
            for (key, value) in [
                ("x-custom-safe", "safe-3"),
                ("authorization", "Bearer attacker"),
                ("x-tenant-id", "tenant-a"),
                ("x-role", "admin"),
                ("invariant-internal-user", "root"),
            ] {
                metadata.insert(key, value.parse().unwrap());
            }
            metadata
        }))
        .unwrap();
    let (url, task) = start_http(server).await;
    let client = reqwest::Client::new();
    let endpoint = format!("{url}/greet.v1.GreetService/Greet");

    let response = client
        .post(&endpoint)
        .header("traceparent", "trace-1")
        .header("x-request-id", "request-2")
        .header("connect-timeout-ms", "1000")
        .json(&json!({"name": "ok"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(response.headers()["x-response-id"], "response-4");
    assert!(!response.headers().contains_key("authorization"));

    let response = client
        .post(&endpoint)
        .header("traceparent", "trace-1")
        .header("x-request-id", "request-2")
        .header("connect-timeout-ms", "1000")
        .json(&json!({"name": "fail"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 400);
    assert_eq!(response.headers()["trailer-x-error-id"], "error-5");
    task.abort();
}

#[tokio::test]
async fn building_http_projection_freezes_registration() {
    let server = registered_server(TestGreetService::default());
    let _router = http_router(server.clone());
    assert_eq!(
        server.set_max_unary_request_bytes(128).unwrap_err().code(),
        Code::FailedPrecondition
    );
}

#[tokio::test]
async fn mcp_streamable_http_subset_enforces_current_transport_contract() {
    let (url, task) = start_http(registered_server(TestGreetService::default())).await;
    let client = reqwest::Client::new();
    let endpoint = format!("{url}/mcp");

    assert_eq!(client.get(&endpoint).send().await.unwrap().status(), 405);
    assert_eq!(
        client
            .get(&endpoint)
            .header("origin", "https://attacker.example")
            .send()
            .await
            .unwrap()
            .status(),
        403
    );

    let initialize = json!({"jsonrpc": "2.0", "id": 1, "method": "initialize"});
    assert_eq!(
        client
            .post(&endpoint)
            .header("content-type", "application/json")
            .header("accept", "application/json")
            .json(&initialize)
            .send()
            .await
            .unwrap()
            .status(),
        406
    );
    assert_eq!(
        client
            .post(&endpoint)
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .header("origin", "null")
            .json(&initialize)
            .send()
            .await
            .unwrap()
            .status(),
        403
    );

    let response = client
        .post(&endpoint)
        .header("content-type", "application/json")
        .header("accept", "application/json, text/event-stream")
        .json(&initialize)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(response.headers()["mcp-protocol-version"], "2025-11-25");
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["result"]["protocolVersion"],
        "2025-11-25"
    );

    for version in [None, Some("2024-11-05"), Some("2026-07-28")] {
        let mut request = client
            .post(&endpoint)
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .json(&json!({"jsonrpc": "2.0", "id": 2, "method": "ping"}));
        if let Some(version) = version {
            request = request.header("mcp-protocol-version", version);
        }
        assert_eq!(request.send().await.unwrap().status(), 400);
    }

    let response = client
        .post(&endpoint)
        .header("content-type", "application/json")
        .header("accept", "application/json, text/event-stream")
        .header("mcp-protocol-version", "2025-11-25")
        .json(&json!({
            "jsonrpc": "2.0",
            "method": "notifications/initialized"
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 202);
    assert!(response.bytes().await.unwrap().is_empty());

    let response = client
        .post(&endpoint)
        .header("content-type", "application/json")
        .header("accept", "application/json, text/event-stream")
        .header("mcp-protocol-version", "2025-11-25")
        .json(&json!({"jsonrpc": "2.0", "id": 3, "result": {}}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 202);
    assert!(response.bytes().await.unwrap().is_empty());

    task.abort();
}
