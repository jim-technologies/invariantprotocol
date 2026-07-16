//! Native gRPC tests use the generated tonic client and service trait only.

mod common;

use common::{
    DESCRIPTOR_PATH, TestGreetService, generated_client, greet, reflected_service_names,
    reflection_has_file_for_symbol, registered_server, serve_native,
};
use invariant::{Code, ErasedRequest, Server, Status};
use prost::Message;
use prost_types::{
    DescriptorProto, FileDescriptorProto, FileDescriptorSet, MethodDescriptorProto,
    ServiceDescriptorProto,
};
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tonic::{Request, Response};
use tonic_types::{ErrorDetails, StatusExt};
use tower::Layer;

#[tokio::test]
async fn generated_registration_client_unary_and_server_streaming_work() {
    let server = registered_server(TestGreetService::default());
    assert_eq!(server.tool_catalog().len(), 3);

    let (address, task) = serve_native(server).await;
    let mut client = generated_client(address).await;

    let response = client
        .greet(greet::GreetRequest {
            name: "World".into(),
            ..Default::default()
        })
        .await
        .unwrap()
        .into_inner();
    assert_eq!(response.message, "Hi World");

    let mut stream = client
        .stream_greet(greet::StreamGreetRequest {
            name: "Stream".into(),
            count: 3,
        })
        .await
        .unwrap()
        .into_inner();
    let mut messages = Vec::new();
    while let Some(message) = stream.message().await.unwrap() {
        messages.push(message.message);
    }
    assert_eq!(messages, ["Hi Stream #0", "Hi Stream #1", "Hi Stream #2"]);
    task.abort();
}

#[tokio::test]
async fn include_exclude_filter_only_optional_projections() {
    let server = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    server.include("greet.v1.GreetService.*").unwrap();
    server.exclude("*GreetGroup").unwrap();
    server.exclude("*StreamGreet").unwrap();
    greet::register_greet_service_server(&server, TestGreetService::default()).unwrap();

    let catalog = server.tool_catalog();
    assert_eq!(catalog.len(), 1);
    assert_eq!(catalog[0]["name"], "GreetService.Greet");
    assert!(server.tool("GreetService.GreetGroup").is_none());

    let (address, task) = serve_native(server).await;
    let mut client = generated_client(address).await;
    let group = client
        .greet_group(greet::GreetGroupRequest {
            people: vec![greet::Person {
                name: "Native".into(),
                ..Default::default()
            }],
        })
        .await
        .unwrap()
        .into_inner();
    assert_eq!(group.messages, ["Hi Native"]);
    let mut stream = client
        .stream_greet(greet::StreamGreetRequest {
            name: "Native".into(),
            count: 1,
        })
        .await
        .unwrap()
        .into_inner();
    assert_eq!(
        stream.message().await.unwrap().unwrap().message,
        "Hi Native #0"
    );
    task.abort();
}

#[tokio::test]
async fn shared_middleware_sees_generated_types_and_canonical_method_once() {
    let server = registered_server(TestGreetService::default());
    let unary_calls = Arc::new(AtomicUsize::new(0));
    let stream_calls = Arc::new(AtomicUsize::new(0));

    let calls = unary_calls.clone();
    server
        .use_shared_unary(Arc::new(move |request: ErasedRequest, info, next| {
            let calls = calls.clone();
            Box::pin(async move {
                calls.fetch_add(1, Ordering::SeqCst);
                assert_eq!(info.full_method, "/greet.v1.GreetService/Greet");
                assert!(request.downcast_ref::<greet::GreetRequest>().is_some());
                let response = next(request).await?;
                assert!(response.downcast_ref::<greet::GreetResponse>().is_some());
                Ok(response)
            })
        }))
        .unwrap();

    let calls = stream_calls.clone();
    server
        .use_shared_stream(Arc::new(move |request: ErasedRequest, info, next| {
            let calls = calls.clone();
            Box::pin(async move {
                calls.fetch_add(1, Ordering::SeqCst);
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

    let (address, task) = serve_native(server).await;
    let mut client = generated_client(address).await;
    client
        .greet(greet::GreetRequest {
            name: "typed".into(),
            ..Default::default()
        })
        .await
        .unwrap();
    let mut stream = client
        .stream_greet(greet::StreamGreetRequest {
            name: "typed".into(),
            count: 2,
        })
        .await
        .unwrap()
        .into_inner();
    while stream.message().await.unwrap().is_some() {}

    assert_eq!(unary_calls.load(Ordering::SeqCst), 1);
    assert_eq!(stream_calls.load(Ordering::SeqCst), 1);
    task.abort();
}

#[tokio::test]
async fn standard_tonic_interceptor_layer_is_native_only_and_runs_once() {
    let server = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    let calls = Arc::new(AtomicUsize::new(0));
    let observed = calls.clone();
    greet::register_greet_service_server_with(
        &server,
        TestGreetService::default(),
        move |native| {
            tonic::service::InterceptorLayer::new(move |request: Request<()>| {
                observed.fetch_add(1, Ordering::SeqCst);
                Ok(request)
            })
            .layer(native)
        },
    )
    .unwrap();
    let projected = server.clone();
    let (address, task) = serve_native(server).await;
    let mut client = generated_client(address).await;
    client
        .greet(greet::GreetRequest {
            name: "native".into(),
            ..Default::default()
        })
        .await
        .unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1);

    let descriptor = projected
        .parsed()
        .pool
        .get_message_by_name("greet.v1.GreetRequest")
        .unwrap();
    let dynamic = prost_reflect::DynamicMessage::decode(
        descriptor,
        greet::GreetRequest {
            name: "projected".into(),
            ..Default::default()
        }
        .encode_to_vec()
        .as_slice(),
    )
    .unwrap();
    projected
        .invoke("GreetService.Greet", Request::new(dynamic))
        .await
        .unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    task.abort();
}

#[tokio::test]
async fn native_metadata_headers_status_details_and_deadline_are_tonic_native() {
    let service = TestGreetService::default().with_greet(|request| async move {
        assert_eq!(request.metadata().get("x-request-id").unwrap(), "request-7");
        if request.get_ref().name == "fail" {
            let mut metadata = tonic::metadata::MetadataMap::new();
            metadata.insert("x-error-id", "error-9".parse().unwrap());
            return Err(Status::with_error_details_and_metadata(
                Code::FailedPrecondition,
                "not ready",
                ErrorDetails::with_bad_request_violation("name", "cannot be fail"),
                metadata,
            ));
        }
        if request.get_ref().name == "slow" {
            assert!(request.metadata().contains_key("grpc-timeout"));
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
        let mut response = Response::new(greet::GreetResponse {
            message: format!("Hi {}", request.get_ref().name),
            ..Default::default()
        });
        response
            .metadata_mut()
            .insert("x-response-id", "response-8".parse().unwrap());
        Ok(response)
    });
    let server = registered_server(service);
    let (address, task) = serve_native(server).await;
    let mut client = generated_client(address).await;

    let mut request = Request::new(greet::GreetRequest {
        name: "ok".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-request-id", "request-7".parse().unwrap());
    let response = client.greet(request).await.unwrap();
    assert_eq!(
        response.metadata().get("x-response-id").unwrap(),
        "response-8"
    );

    let mut request = Request::new(greet::GreetRequest {
        name: "fail".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-request-id", "request-7".parse().unwrap());
    let status = client.greet(request).await.unwrap_err();
    assert_eq!(status.code(), Code::FailedPrecondition);
    assert_eq!(status.message(), "not ready");
    assert_eq!(status.metadata().get("x-error-id").unwrap(), "error-9");
    let detail = status.get_details_bad_request().unwrap();
    assert_eq!(detail.field_violations[0].field, "name");
    assert_eq!(detail.field_violations[0].description, "cannot be fail");

    let mut request = Request::new(greet::GreetRequest {
        name: "slow".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-request-id", "request-7".parse().unwrap());
    request.set_timeout(Duration::from_millis(20));
    // Tonic 0.14 maps its transport TimeoutExpired to Cancelled. The timeout
    // metadata reached the generated handler and the handler future was
    // cancelled by tonic's normal transport layer.
    assert_eq!(
        client.greet(request).await.unwrap_err().code(),
        Code::Cancelled
    );
    task.abort();
}

#[tokio::test]
async fn generated_service_configuration_enforces_native_message_limits() {
    let server = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    let service = TestGreetService::default().with_greet(|request| async move {
        let name = request.into_inner().name;
        Ok(Response::new(greet::GreetResponse {
            message: if name == "large-response" {
                "y".repeat(64)
            } else {
                name
            },
            ..Default::default()
        }))
    });
    greet::register_greet_service_server_with(&server, service, |native| {
        native
            .max_decoding_message_size(32)
            .max_encoding_message_size(32)
    })
    .unwrap();
    let (address, task) = serve_native(server).await;
    let mut client = generated_client(address).await;

    let status = client
        .greet(greet::GreetRequest {
            name: "x".repeat(64),
            ..Default::default()
        })
        .await
        .unwrap_err();
    assert_eq!(status.code(), Code::OutOfRange);

    let status = client
        .greet(greet::GreetRequest {
            name: "large-response".into(),
            ..Default::default()
        })
        .await
        .unwrap_err();
    assert_eq!(status.code(), Code::OutOfRange);
    task.abort();
}

#[tokio::test]
async fn reflection_exposes_only_complete_registered_generated_services() {
    let mut descriptors =
        FileDescriptorSet::decode(std::fs::read(DESCRIPTOR_PATH).unwrap().as_slice()).unwrap();
    descriptors.file.push(FileDescriptorProto {
        name: Some("hidden/v1/hidden.proto".into()),
        package: Some("hidden.v1".into()),
        syntax: Some("proto3".into()),
        message_type: vec![
            DescriptorProto {
                name: Some("Input".into()),
                ..Default::default()
            },
            DescriptorProto {
                name: Some("Output".into()),
                ..Default::default()
            },
        ],
        service: vec![ServiceDescriptorProto {
            name: Some("HiddenService".into()),
            method: vec![MethodDescriptorProto {
                name: Some("Hidden".into()),
                input_type: Some(".hidden.v1.Input".into()),
                output_type: Some(".hidden.v1.Output".into()),
                ..Default::default()
            }],
            ..Default::default()
        }],
        ..Default::default()
    });
    let server = Arc::new(Server::from_bytes(&descriptors.encode_to_vec()).unwrap());
    greet::register_greet_service_server(&server, TestGreetService::default()).unwrap();
    let (address, task) = serve_native(server).await;

    assert_eq!(
        reflected_service_names(address).await,
        [
            "grpc.reflection.v1.ServerReflection".to_string(),
            "greet.v1.GreetService".to_string(),
        ]
        .into_iter()
        .collect()
    );
    assert!(reflection_has_file_for_symbol(address, "greet.v1.GreetService").await);
    assert!(!reflection_has_file_for_symbol(address, "hidden.v1.HiddenService").await);
    task.abort();
}

#[tokio::test]
async fn caller_composes_tonic_graceful_shutdown_after_route_extraction() {
    let started = Arc::new(tokio::sync::Notify::new());
    let release = Arc::new(tokio::sync::Notify::new());
    let service = TestGreetService::default().with_greet({
        let started = started.clone();
        let release = release.clone();
        move |request| {
            let started = started.clone();
            let release = release.clone();
            async move {
                started.notify_one();
                release.notified().await;
                Ok(Response::new(greet::GreetResponse {
                    message: request.into_inner().name,
                    ..Default::default()
                }))
            }
        }
    });
    let server = registered_server(service);
    let routes = server.grpc_routes();
    assert_eq!(
        server.exclude("*").unwrap_err().code(),
        Code::FailedPrecondition
    );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, shutdown_requested) = tokio::sync::oneshot::channel();
    let serve = tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_routes(routes)
            .serve_with_incoming_shutdown(
                tokio_stream::wrappers::TcpListenerStream::new(listener),
                async {
                    let _ = shutdown_requested.await;
                },
            )
            .await
            .unwrap();
    });
    let mut client = generated_client(address).await;
    let call = tokio::spawn(async move {
        client
            .greet(greet::GreetRequest {
                name: "drained".into(),
                ..Default::default()
            })
            .await
            .unwrap()
            .into_inner()
    });
    started.notified().await;
    shutdown.send(()).unwrap();
    tokio::task::yield_now().await;
    assert!(!serve.is_finished());
    release.notify_one();
    assert_eq!(call.await.unwrap().message, "drained");
    tokio::time::timeout(Duration::from_secs(1), serve)
        .await
        .unwrap()
        .unwrap();
}

#[test]
fn generated_registration_rejects_descriptor_drift_and_late_registration() {
    let bytes = std::fs::read(DESCRIPTOR_PATH).unwrap();
    let mut descriptor = FileDescriptorSet::decode(bytes.as_slice()).unwrap();
    let greet_file = descriptor
        .file
        .iter_mut()
        .find(|file| file.package.as_deref() == Some("greet.v1"))
        .unwrap();
    greet_file.message_type[0].field[0].name = Some("renamed".into());
    let server = Server::from_bytes(&descriptor.encode_to_vec()).unwrap();
    assert_eq!(
        greet::register_greet_service_server(&server, TestGreetService::default())
            .unwrap_err()
            .code(),
        Code::FailedPrecondition
    );

    let server = registered_server(TestGreetService::default());
    server.grpc_routes();
    assert_eq!(
        greet::register_greet_service_server(&server, TestGreetService::default())
            .unwrap_err()
            .code(),
        Code::FailedPrecondition
    );
}
