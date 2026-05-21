//! gRPC projection integration test — descriptor-driven dispatch via tonic
//! transport. Uses tonic's dynamic client (`Grpc::unary`) to confirm a real
//! gRPC client over HTTP/2 prior-knowledge handshakes successfully.

mod common;

use common::{greet, DESCRIPTOR_PATH};
use futures::StreamExt;
use invariant::projections::grpc::grpc_routes;
use invariant::{Server, ServerStreamTx, Status};
use std::sync::Arc;

async fn greet_handler(req: greet::GreetRequest) -> Result<greet::GreetResponse, Status> {
    Ok(greet::GreetResponse {
        message: format!("Hi {}", req.name),
        ..Default::default()
    })
}

#[tokio::test]
async fn grpc_unary_roundtrip() {
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).unwrap();
    srv.register_unary("GreetService.Greet", greet_handler);
    let routes = grpc_routes(Arc::new(srv));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        let _ = tonic::transport::Server::builder()
            .add_routes(routes)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });
    // Settle for tonic to start accepting HTTP/2 connections.
    tokio::time::sleep(std::time::Duration::from_millis(80)).await;

    let channel = tonic::transport::Channel::from_shared(format!("http://{addr}"))
        .unwrap()
        .connect()
        .await
        .unwrap();

    let mut client = tonic::client::Grpc::new(channel);
    client.ready().await.unwrap();

    let req = tonic::Request::new(greet::GreetRequest {
        name: "World".into(),
        ..Default::default()
    });
    let path: http::uri::PathAndQuery = "/greet.v1.GreetService/Greet".parse().unwrap();
    let codec = tonic::codec::ProstCodec::<greet::GreetRequest, greet::GreetResponse>::default();
    let resp = client.unary(req, path, codec).await.unwrap();
    assert_eq!(resp.into_inner().message, "Hi World");

    handle.abort();
}

async fn stream_greet(
    req: greet::StreamGreetRequest,
    tx: ServerStreamTx<greet::GreetResponse>,
) -> Result<(), Status> {
    let n = if req.count <= 0 { 1 } else { req.count };
    for i in 0..n {
        tx.send(greet::GreetResponse {
            message: format!("Hi {} #{}", req.name, i),
            ..Default::default()
        })
        .await?;
    }
    Ok(())
}

#[tokio::test]
async fn grpc_reflection_lists_registered_service() {
    // gRPC reflection (grpc.reflection.v1.ServerReflection) must surface our
    // registered services. Mirrors Go's `TestGRPCReflection` /
    // Python's `test_grpc_reflection`.
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).unwrap();
    srv.register_unary("GreetService.Greet", greet_handler);
    let server = Arc::new(srv);
    let reflection = invariant::projections::grpc::build_reflection(&server).unwrap();
    let routes = invariant::projections::grpc::grpc_routes(server);

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        let _ = tonic::transport::Server::builder()
            .add_routes(routes)
            .add_service(reflection)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });
    tokio::time::sleep(std::time::Duration::from_millis(80)).await;

    let channel = tonic::transport::Channel::from_shared(format!("http://{addr}"))
        .unwrap()
        .connect()
        .await
        .unwrap();

    let mut client =
        tonic_reflection::pb::v1::server_reflection_client::ServerReflectionClient::new(channel);
    let (tx, rx) = tokio::sync::mpsc::channel(1);
    tx.send(tonic_reflection::pb::v1::ServerReflectionRequest {
        host: String::new(),
        message_request: Some(
            tonic_reflection::pb::v1::server_reflection_request::MessageRequest::ListServices(
                String::new(),
            ),
        ),
    })
    .await
    .unwrap();
    drop(tx);
    let resp = client
        .server_reflection_info(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .unwrap();
    let mut stream = resp.into_inner();
    let mut services: Vec<String> = Vec::new();
    while let Some(item) = stream.next().await {
        let item = item.unwrap();
        if let Some(tonic_reflection::pb::v1::server_reflection_response::MessageResponse::ListServicesResponse(ls)) =
            item.message_response
        {
            for svc in ls.service {
                services.push(svc.name);
            }
        }
    }
    assert!(
        services.iter().any(|s| s == "greet.v1.GreetService"),
        "expected greet.v1.GreetService in reflection list, got: {services:?}"
    );

    handle.abort();
}

#[tokio::test]
async fn grpc_server_streaming_roundtrip() {
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).unwrap();
    srv.register_stream("GreetService.StreamGreet", stream_greet);
    let routes = grpc_routes(Arc::new(srv));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        let _ = tonic::transport::Server::builder()
            .add_routes(routes)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });
    tokio::time::sleep(std::time::Duration::from_millis(80)).await;

    let channel = tonic::transport::Channel::from_shared(format!("http://{addr}"))
        .unwrap()
        .connect()
        .await
        .unwrap();

    let mut client = tonic::client::Grpc::new(channel);
    client.ready().await.unwrap();

    let req = tonic::Request::new(greet::StreamGreetRequest {
        name: "Stream".into(),
        count: 3,
    });
    let path: http::uri::PathAndQuery = "/greet.v1.GreetService/StreamGreet".parse().unwrap();
    let codec =
        tonic::codec::ProstCodec::<greet::StreamGreetRequest, greet::GreetResponse>::default();
    let mut stream = client
        .server_streaming(req, path, codec)
        .await
        .unwrap()
        .into_inner();

    let mut msgs = Vec::new();
    while let Some(msg) = stream.next().await {
        msgs.push(msg.unwrap().message);
    }
    assert_eq!(msgs, vec!["Hi Stream #0", "Hi Stream #1", "Hi Stream #2"]);

    handle.abort();
}
