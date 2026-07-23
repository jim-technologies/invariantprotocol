//! Benchmarks — mirror `go/benchmarks_test.go` so the implementations
//! can be compared on identical workloads.
//!
//! Run with: `cargo bench --bench bench`

use criterion::{Criterion, criterion_group, criterion_main};
use futures::StreamExt;
use invariant::{Request, Server, projections::http::http_router};
use prost::Message;
use prost_reflect::DynamicMessage;
use std::hint::black_box;
use std::sync::Arc;

#[path = "../tests/common/mod.rs"]
mod common;
use common::{TestGreetService, greet, registered_server};

fn build_server() -> Arc<Server> {
    registered_server(TestGreetService::default())
}

fn bench_invoke_stream_direct(c: &mut Criterion) {
    let rt = tokio::runtime::Runtime::new().unwrap();
    let server = build_server();
    let pool = &server.parsed().pool;
    let desc = pool
        .get_message_by_name("greet.v1.StreamGreetRequest")
        .unwrap();
    let req_typed = greet::StreamGreetRequest {
        name: "World".into(),
        count: 10,
    };
    let req_bytes = req_typed.encode_to_vec();

    c.bench_function("invoke_stream_direct_10", |b| {
        b.to_async(&rt).iter(|| async {
            let dyn_req = DynamicMessage::decode(desc.clone(), &req_bytes[..]).unwrap();
            let mut stream = server
                .invoke_stream("greet.v1.GreetService.StreamGreet", Request::new(dyn_req))
                .await
                .unwrap()
                .into_inner();
            let mut n = 0;
            while let Some(item) = stream.next().await {
                let _ = item.unwrap();
                n += 1;
            }
            black_box(n);
        });
    });
}

fn bench_invoke_direct(c: &mut Criterion) {
    let rt = tokio::runtime::Runtime::new().unwrap();
    let server = build_server();
    let pool = &server.parsed().pool;
    let desc = pool.get_message_by_name("greet.v1.GreetRequest").unwrap();
    let req_typed = greet::GreetRequest {
        name: "World".into(),
        ..Default::default()
    };
    let req_bytes = req_typed.encode_to_vec();

    c.bench_function("invoke_direct", |b| {
        b.to_async(&rt).iter(|| async {
            let dyn_req = DynamicMessage::decode(desc.clone(), &req_bytes[..]).unwrap();
            let resp = server
                .invoke("greet.v1.GreetService.Greet", Request::new(dyn_req))
                .await
                .unwrap();
            black_box(resp);
        });
    });
}

fn bench_http_json(c: &mut Criterion) {
    let rt = tokio::runtime::Runtime::new().unwrap();
    let server = build_server();

    let (url, _handle) = rt.block_on(async {
        let app = http_router(server.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let handle = tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        tokio::task::yield_now().await;
        (format!("http://{addr}/greet.v1.GreetService/Greet"), handle)
    });

    let client = reqwest::Client::new();
    let body = serde_json::json!({"name": "World"});
    c.bench_function("http_json", |b| {
        b.to_async(&rt).iter(|| async {
            let resp = client.post(&url).json(&body).send().await.unwrap();
            black_box(resp.bytes().await.unwrap());
        });
    });
}

fn bench_http_proto(c: &mut Criterion) {
    let rt = tokio::runtime::Runtime::new().unwrap();
    let server = build_server();

    let (url, _handle) = rt.block_on(async {
        let app = http_router(server.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let handle = tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        tokio::task::yield_now().await;
        (format!("http://{addr}/greet.v1.GreetService/Greet"), handle)
    });

    let client = reqwest::Client::new();
    let body = greet::GreetRequest {
        name: "World".into(),
        ..Default::default()
    }
    .encode_to_vec();
    c.bench_function("http_proto", |b| {
        b.to_async(&rt).iter(|| async {
            let resp = client
                .post(&url)
                .header("content-type", "application/proto")
                .header("accept", "application/proto")
                .body(body.clone())
                .send()
                .await
                .unwrap();
            black_box(resp.bytes().await.unwrap());
        });
    });
}

criterion_group!(
    benches,
    bench_invoke_direct,
    bench_invoke_stream_direct,
    bench_http_json,
    bench_http_proto
);
criterion_main!(benches);
