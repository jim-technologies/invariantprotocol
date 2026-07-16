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
use tonic_types::{ErrorDetails, StatusExt};

async fn start_http(server: Arc<invariant::Server>) -> (String, tokio::task::JoinHandle<()>) {
    let app = http_router(server);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{address}"), task)
}

fn mcp_initialize(protocol_version: &str) -> serde_json::Value {
    json!({
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {
            "protocolVersion": protocol_version,
            "capabilities": {},
            "clientInfo": {"name": "invariant-test", "version": "1.0"},
        },
    })
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
            if request.get_ref().name == "fast" {
                return Ok(Response::new(greet::GreetResponse {
                    message: "fast".into(),
                    ..Default::default()
                }));
            }
            if request.get_ref().name == "cpu" {
                let deadline = std::time::Instant::now() + Duration::from_millis(20);
                while std::time::Instant::now() < deadline {
                    std::hint::spin_loop();
                }
                return Ok(Response::new(greet::GreetResponse {
                    message: "too late".into(),
                    ..Default::default()
                }));
            }
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

    let response = client
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .header("connect-timeout-ms", "1")
        .json(&serde_json::json!({"name": "cpu"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 504);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "deadline_exceeded"
    );

    for invalid_timeout in ["0", "-1", "+1", "1.0", "abc", "12345678901"] {
        let response = client
            .post(format!("{url}/greet.v1.GreetService/Greet"))
            .header("connect-timeout-ms", invalid_timeout)
            .json(&serde_json::json!({"name": "fast"}))
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 400, "{invalid_timeout:?}");
        assert_eq!(
            response.json::<serde_json::Value>().await.unwrap()["code"],
            "invalid_argument",
            "{invalid_timeout:?}"
        );
    }

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
async fn http_requires_canonical_content_types_and_rejects_unsupported_encoding() {
    let (url, task) = start_http(registered_server(TestGreetService::default())).await;
    let client = reqwest::Client::new();
    let unary = format!("{url}/greet.v1.GreetService/Greet");

    for content_type in [None, Some("text/plain"), Some("application/connect+json")] {
        let mut request = client.post(&unary).body(r#"{"name":"Ada"}"#);
        if let Some(content_type) = content_type {
            request = request.header("content-type", content_type);
        }
        let response = request.send().await.unwrap();
        assert_eq!(response.status(), 415);
        assert_eq!(
            response.json::<serde_json::Value>().await.unwrap()["code"],
            "invalid_argument"
        );
    }

    let response = client
        .post(&unary)
        .header("content-type", "application/json")
        .header("content-encoding", "gzip")
        .body(r#"{"name":"Ada"}"#)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 501);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "unimplemented"
    );

    let stream = format!("{url}/greet.v1.GreetService/StreamGreet");
    for content_type in [None, Some("application/json")] {
        let mut request = client.post(&stream).body(Vec::<u8>::new());
        if let Some(content_type) = content_type {
            request = request.header("content-type", content_type);
        }
        let response = request.send().await.unwrap();
        assert_eq!(response.status(), 415);
        assert_eq!(
            response.json::<serde_json::Value>().await.unwrap()["code"],
            "invalid_argument"
        );
    }
    task.abort();
}

#[tokio::test]
async fn unary_error_responses_are_bounded_by_the_configured_response_limit() {
    fn rich_error_service() -> TestGreetService {
        TestGreetService::default().with_greet(|_| async {
            Err(Status::with_error_details(
                Code::InvalidArgument,
                "invalid",
                ErrorDetails::with_bad_request_violation("name", "x".repeat(4096)),
            ))
        })
    }

    let limited = registered_server(rich_error_service());
    limited.set_max_unary_response_bytes(160).unwrap();
    let (url, task) = start_http(limited).await;
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&json!({"name": "Ada"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    let body = response.bytes().await.unwrap();
    assert!(body.len() <= 160);
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&body).unwrap()["code"],
        "resource_exhausted"
    );
    task.abort();

    let tiny = registered_server(rich_error_service());
    tiny.set_max_unary_response_bytes(1).unwrap();
    let (url, task) = start_http(tiny).await;
    let response = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/Greet"))
        .json(&json!({"name": "Ada"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    assert!(response.bytes().await.unwrap().is_empty());
    task.abort();
}

#[tokio::test]
async fn http_middleware_extensions_reach_the_tonic_request() {
    #[derive(Clone, Debug, PartialEq, Eq)]
    struct TrustedPrincipal(&'static str);

    let service = TestGreetService::default().with_greet(|request| async move {
        assert_eq!(
            request.extensions().get::<TrustedPrincipal>(),
            Some(&TrustedPrincipal("alice"))
        );
        Ok(Response::new(greet::GreetResponse {
            message: "authenticated".into(),
            ..Default::default()
        }))
    });
    let server = registered_server(service);
    let app = http_router(server).layer(axum::middleware::from_fn(
        |mut request: axum::extract::Request, next: axum::middleware::Next| async move {
            request.extensions_mut().insert(TrustedPrincipal("alice"));
            next.run(request).await
        },
    ));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    let response = reqwest::Client::new()
        .post(format!("http://{address}/greet.v1.GreetService/Greet"))
        .json(&json!({"name": "Ada"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
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
            "authorization-bin",
            "proxy-authorization-bin",
            "authentication-bin",
            "api-key-bin",
            "x-api-key-bin",
            "cookie-bin",
            "set-cookie-bin",
            "x-tenant-id",
            "x-role",
            "invariant-internal-user",
        ] {
            assert!(!request.metadata().contains_key(denied));
        }
        assert_eq!(
            request
                .metadata()
                .get_bin("trace-bin")
                .unwrap()
                .to_bytes()
                .unwrap(),
            b"\x01\x02".as_slice()
        );
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
            .insert("authorization", "Bearer trusted-handler".parse().unwrap());
        response
            .metadata_mut()
            .insert("x-tenant", "trusted-tenant".parse().unwrap());
        response
            .metadata_mut()
            .insert("set-cookie", "session=trusted".parse().unwrap());
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
            for key in [
                "trace-bin",
                "authorization-bin",
                "proxy-authorization-bin",
                "authentication-bin",
                "api-key-bin",
                "x-api-key-bin",
                "cookie-bin",
                "set-cookie-bin",
            ] {
                metadata.insert_bin(key, tonic::metadata::MetadataValue::from_bytes(b"\x01\x02"));
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
    assert_eq!(
        response.headers()["authorization"],
        "Bearer trusted-handler"
    );
    assert_eq!(response.headers()["x-tenant"], "trusted-tenant");
    assert_eq!(response.headers()["set-cookie"], "session=trusted");

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

    let initialize = mcp_initialize("2025-11-25");
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
    for accept in [
        "application/json, text/event-stream;q=0",
        "application/json, text/event-stream;q=bogus",
    ] {
        assert_eq!(
            client
                .post(&endpoint)
                .header("content-type", "application/json")
                .header("accept", accept)
                .json(&initialize)
                .send()
                .await
                .unwrap()
                .status(),
            406
        );
    }

    let response = client
        .post(&endpoint)
        .header("content-type", "application/json")
        .header("accept", "APPLICATION/JSON, TEXT/EVENT-STREAM")
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
    let response = client
        .post(&endpoint)
        .header("content-type", "application/json")
        .header("accept", "application/json, text/event-stream")
        .json(&mcp_initialize("2099-01-01"))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["result"]["protocolVersion"],
        "2025-11-25"
    );

    for invalid in [
        json!(42),
        json!([]),
        json!({"id": 1, "method": "ping"}),
        json!({"jsonrpc": "1.0", "id": 2, "method": "ping"}),
        json!({"jsonrpc": "2.0", "id": 3}),
        json!({"jsonrpc": "2.0", "id": 4, "method": 7}),
        json!({"jsonrpc": "2.0", "id": null, "method": "ping"}),
        json!({"jsonrpc": "2.0", "id": true, "method": "ping"}),
        json!({"jsonrpc": "2.0", "id": 1.5, "method": "ping"}),
        json!({"jsonrpc": "2.0", "id": 9_007_199_254_740_992_i64, "method": "ping"}),
        json!({"jsonrpc": "2.0", "id": -9_007_199_254_740_992_i64, "method": "ping"}),
        json!({"jsonrpc": "2.0", "id": 5, "result": "not-an-object"}),
        json!({"jsonrpc": "2.0", "id": 5, "result": {}, "error": {}}),
        json!({"jsonrpc": "2.0", "result": {}}),
        json!({"jsonrpc": "2.0", "id": null, "error": {"code": -32601, "message": "missing"}}),
        json!({"jsonrpc": "2.0", "error": {"code": 1.5, "message": "bad code"}}),
    ] {
        let response = client
            .post(&endpoint)
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .json(&invalid)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 200);
        assert_eq!(
            response.json::<serde_json::Value>().await.unwrap()["error"]["code"],
            -32600
        );
    }

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

    for client_response in [
        json!({"jsonrpc": "2.0", "id": 3, "result": {}}),
        json!({"jsonrpc": "2.0", "error": {"code": -32601, "message": "unknown request"}}),
    ] {
        let response = client
            .post(&endpoint)
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .header("mcp-protocol-version", "2025-11-25")
            .json(&client_response)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 202);
        assert!(response.bytes().await.unwrap().is_empty());
    }

    for request in [
        json!({"jsonrpc": "2.0", "id": 4, "method": "ping", "params": []}),
        json!({"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": {"name": [], "arguments": {}}}),
        json!({"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": {"name": "GreetService.Greet", "arguments": []}}),
    ] {
        let response = client
            .post(&endpoint)
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .header("mcp-protocol-version", "2025-11-25")
            .json(&request)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 200);
        assert_eq!(
            response.json::<serde_json::Value>().await.unwrap()["error"]["code"],
            -32602
        );
    }

    task.abort();
}

#[tokio::test]
async fn mcp_http_responses_use_the_global_unary_response_limit() {
    let limited = registered_server(TestGreetService::default());
    limited.set_max_unary_response_bytes(160).unwrap();
    let (url, task) = start_http(limited).await;
    let response = reqwest::Client::new()
        .post(format!("{url}/mcp"))
        .header("content-type", "application/json")
        .header("accept", "application/json, text/event-stream")
        .header("mcp-protocol-version", "2025-11-25")
        .json(&json!({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    assert_eq!(
        response.json::<serde_json::Value>().await.unwrap()["code"],
        "resource_exhausted"
    );
    task.abort();

    let tiny = registered_server(TestGreetService::default());
    tiny.set_max_unary_response_bytes(1).unwrap();
    let (url, task) = start_http(tiny).await;
    let response = reqwest::Client::new()
        .post(format!("{url}/mcp"))
        .header("content-type", "application/json")
        .header("accept", "application/json, text/event-stream")
        .body("{bad json")
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 429);
    assert!(response.bytes().await.unwrap().is_empty());
    task.abort();
}

#[tokio::test]
async fn mcp_http_transport_timeout_is_a_bounded_connect_error() {
    let service = TestGreetService::default().with_greet(|_| async {
        let deadline = std::time::Instant::now() + Duration::from_millis(20);
        while std::time::Instant::now() < deadline {
            std::hint::spin_loop();
        }
        Ok(Response::new(greet::GreetResponse::default()))
    });
    let server = registered_server(service);
    server.set_max_unary_response_bytes(160).unwrap();
    let (url, task) = start_http(server).await;
    let response = reqwest::Client::new()
        .post(format!("{url}/mcp"))
        .header("content-type", "application/json")
        .header("accept", "application/json, text/event-stream")
        .header("mcp-protocol-version", "2025-11-25")
        .header("connect-timeout-ms", "1")
        .json(&json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": {
                "name": "GreetService.Greet",
                "arguments": {"name": "slow"}
            }
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 504);
    let body = response.bytes().await.unwrap();
    assert!(body.len() <= 160);
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&body).unwrap()["code"],
        "deadline_exceeded"
    );
    task.abort();
}

#[tokio::test]
async fn mcp_http_rejects_malformed_connect_timeouts() {
    let (url, task) = start_http(registered_server(TestGreetService::default())).await;
    let client = reqwest::Client::new();
    for invalid_timeout in ["0", "-1", "+1", "1.0", "abc", "12345678901"] {
        let response = client
            .post(format!("{url}/mcp"))
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .header("connect-timeout-ms", invalid_timeout)
            .json(&mcp_initialize("2025-11-25"))
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 400, "{invalid_timeout:?}");
        assert_eq!(
            response.json::<serde_json::Value>().await.unwrap()["code"],
            "invalid_argument",
            "{invalid_timeout:?}"
        );
    }
    task.abort();
}
