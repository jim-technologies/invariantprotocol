//! Remote unary projections use caller-owned transports without an in-memory hop.

mod common;

use common::{
    DESCRIPTOR_PATH, TestGreetService, generated_client, greet, reflected_method_names,
    reflected_service_names, serve_native,
};
use invariant::{Code, ProjectionContext, Response, Server, Status};
use prost::Message;
use prost_types::{
    DescriptorProto, FileDescriptorProto, FileDescriptorSet, MethodDescriptorProto,
    ServiceDescriptorProto,
};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tonic::Request;
use tonic_types::{ErrorDetails, StatusExt};

async fn start_http(server: Arc<Server>) -> (reqwest::Url, tokio::task::JoinHandle<()>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, invariant::projections::http::http_router(server))
            .await
            .unwrap();
    });
    (
        reqwest::Url::parse(&format!("http://{address}")).unwrap(),
        task,
    )
}

fn colliding_remote_descriptor() -> Vec<u8> {
    let file = |package: &str| FileDescriptorProto {
        name: Some(format!("{package}/echo.proto")),
        package: Some(package.into()),
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
            name: Some("EchoService".into()),
            method: vec![MethodDescriptorProto {
                name: Some("Call".into()),
                input_type: Some(format!(".{package}.Input")),
                output_type: Some(format!(".{package}.Output")),
                ..Default::default()
            }],
            ..Default::default()
        }],
        ..Default::default()
    };
    FileDescriptorSet {
        file: vec![file("alpha.v1"), file("beta.v1")],
    }
    .encode_to_vec()
}

#[tokio::test]
async fn batch_remote_registration_rejects_tool_collisions_atomically() {
    let descriptor = colliding_remote_descriptor();
    let http = Server::from_bytes(&descriptor).unwrap();
    let client = reqwest::Client::new();
    let base_url = reqwest::Url::parse("https://example.test").unwrap();
    let status = http.connect_http(&client, base_url.clone()).unwrap_err();
    assert_eq!(status.code(), Code::AlreadyExists);
    assert!(status.message().contains("EchoService.Call"));
    assert!(http.tool_catalog().is_empty());

    http.exclude("beta.v1.*").unwrap();
    http.connect_http(&client, base_url).unwrap();
    assert_eq!(http.tool_catalog().len(), 1);
    assert_eq!(http.tool_catalog()[0]["name"], "EchoService.Call");

    let grpc = Server::from_bytes(&descriptor).unwrap();
    let channel = tonic::transport::Endpoint::from_static("http://127.0.0.1:1").connect_lazy();
    let status = grpc
        .connect_grpc(channel.clone(), |client| client)
        .unwrap_err();
    assert_eq!(status.code(), Code::AlreadyExists);
    assert!(status.message().contains("EchoService.Call"));
    assert!(grpc.tool_catalog().is_empty());

    grpc.exclude("beta.v1.*").unwrap();
    grpc.connect_grpc(channel, |client| client).unwrap();
    assert_eq!(grpc.tool_catalog().len(), 1);
    assert_eq!(grpc.tool_catalog()[0]["name"], "EchoService.Call");
}

#[test]
fn remote_http_registration_accepts_https_with_rustls_enabled() {
    let server = Server::from_descriptor(DESCRIPTOR_PATH).unwrap();
    let client = reqwest::Client::builder().build().unwrap();
    server
        .connect_http(
            &client,
            reqwest::Url::parse("https://api.example.test/base").unwrap(),
        )
        .unwrap();
    assert_eq!(server.tool_catalog().len(), 2);
}

#[tokio::test]
async fn remote_unary_proxy_reflects_exactly_the_served_methods() {
    let channel = tonic::transport::Endpoint::from_static("http://127.0.0.1:1").connect_lazy();
    let proxy = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    proxy.connect_grpc(channel, |client| client).unwrap();
    let (address, task) = serve_native(proxy).await;

    assert_eq!(
        reflected_service_names(address).await,
        [
            "grpc.reflection.v1.ServerReflection".to_string(),
            "greet.v1.GreetService".to_string(),
        ]
        .into_iter()
        .collect()
    );
    assert_eq!(
        reflected_method_names(address, "greet.v1.GreetService").await,
        ["Greet".to_string(), "GreetGroup".to_string()]
            .into_iter()
            .collect()
    );
    task.abort();
}

#[tokio::test]
async fn caller_owned_tonic_transport_preserves_unary_semantics_and_controls() {
    let saw_timeout = Arc::new(AtomicBool::new(false));
    let observed = saw_timeout.clone();
    let backend =
        common::registered_server(TestGreetService::default().with_greet(move |request| {
            let observed = observed.clone();
            async move {
                assert_eq!(request.metadata().get("x-client-id").unwrap(), "client-1");
                match request.get_ref().name.as_str() {
                    "fail" => {
                        let mut metadata = tonic::metadata::MetadataMap::new();
                        metadata.insert("x-error-id", "grpc-error-2".parse().unwrap());
                        Err(Status::with_error_details_and_metadata(
                            Code::FailedPrecondition,
                            "remote failure",
                            ErrorDetails::with_bad_request_violation("name", "cannot fail"),
                            metadata,
                        ))
                    }
                    "large" => Ok(Response::new(greet::GreetResponse {
                        message: "x".repeat(512),
                        ..Default::default()
                    })),
                    "slow" => {
                        observed.store(
                            request.metadata().contains_key("grpc-timeout"),
                            Ordering::SeqCst,
                        );
                        tokio::time::sleep(Duration::from_secs(1)).await;
                        Ok(Response::new(greet::GreetResponse::default()))
                    }
                    name => {
                        let mut response = Response::new(greet::GreetResponse {
                            message: format!("Remote {name}"),
                            ..Default::default()
                        });
                        response
                            .metadata_mut()
                            .insert("x-backend-id", "grpc-backend-3".parse().unwrap());
                        Ok(response)
                    }
                }
            }
        }));
    let (backend_address, backend_task) = serve_native(backend).await;
    let channel = tonic::transport::Endpoint::from_shared(format!("http://{backend_address}"))
        .unwrap()
        .connect()
        .await
        .unwrap();

    let proxy = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    proxy
        .connect_grpc(channel.clone(), |client| {
            client.max_decoding_message_size(256)
        })
        .unwrap();
    assert_eq!(proxy.tool_catalog().len(), 2);
    let (proxy_address, proxy_task) = serve_native(proxy).await;
    let mut client = generated_client(proxy_address).await;

    let mut request = Request::new(greet::GreetRequest {
        name: "ok".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-client-id", "client-1".parse().unwrap());
    let response = client.greet(request).await.unwrap();
    assert_eq!(response.get_ref().message, "Remote ok");
    assert_eq!(
        response.metadata().get("x-backend-id").unwrap(),
        "grpc-backend-3"
    );

    let mut request = Request::new(greet::GreetRequest {
        name: "fail".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-client-id", "client-1".parse().unwrap());
    let status = client.greet(request).await.unwrap_err();
    assert_eq!(status.code(), Code::FailedPrecondition);
    assert_eq!(status.metadata().get("x-error-id").unwrap(), "grpc-error-2");
    assert_eq!(
        status.get_details_bad_request().unwrap().field_violations[0].field,
        "name"
    );

    let mut request = Request::new(greet::GreetRequest {
        name: "large".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-client-id", "client-1".parse().unwrap());
    assert_eq!(
        client.greet(request).await.unwrap_err().code(),
        Code::OutOfRange
    );

    let mut request = Request::new(greet::GreetRequest {
        name: "slow".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-client-id", "client-1".parse().unwrap());
    request.set_timeout(Duration::from_millis(50));
    assert!(matches!(
        client.greet(request).await.unwrap_err().code(),
        Code::Cancelled | Code::DeadlineExceeded
    ));
    tokio::time::timeout(Duration::from_secs(1), async {
        while !saw_timeout.load(Ordering::SeqCst) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();

    // The caller still owns and can independently use the shared channel.
    let mut backend_client = greet::greet_service_client::GreetServiceClient::new(channel);
    let mut request = Request::new(greet::GreetRequest {
        name: "owned".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-client-id", "client-1".parse().unwrap());
    assert_eq!(
        backend_client
            .greet(request)
            .await
            .unwrap()
            .into_inner()
            .message,
        "Remote owned"
    );
    proxy_task.abort();
    backend_task.abort();
}

#[tokio::test]
async fn caller_owned_reqwest_transport_preserves_metadata_details_deadlines_and_bounds() {
    let saw_timeout = Arc::new(AtomicBool::new(false));
    let observed = saw_timeout.clone();
    let backend =
        common::registered_server(TestGreetService::default().with_greet(move |request| {
            let observed = observed.clone();
            async move {
                assert_eq!(request.metadata().get("x-request-id").unwrap(), "request-4");
                if request.get_ref().name == "ok" {
                    assert_eq!(
                        request.metadata().get("traceparent").unwrap(),
                        "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
                    );
                    assert!(!request.metadata().contains_key("authorization"));
                    assert!(!request.metadata().contains_key("x-tenant"));
                    assert!(!request.metadata().contains_key("x-role"));
                }
                match request.get_ref().name.as_str() {
                    "fail" => {
                        let mut metadata = tonic::metadata::MetadataMap::new();
                        metadata.insert("x-error-id", "http-error-5".parse().unwrap());
                        Err(Status::with_error_details_and_metadata(
                            Code::FailedPrecondition,
                            "remote HTTP failure",
                            ErrorDetails::with_bad_request_violation("name", "cannot fail"),
                            metadata,
                        ))
                    }
                    "large" => Ok(Response::new(greet::GreetResponse {
                        message: "y".repeat(1024),
                        ..Default::default()
                    })),
                    "slow" => {
                        let context = request.extensions().get::<ProjectionContext>().unwrap();
                        observed.store(
                            context.deadline().is_some()
                                && request.metadata().contains_key("grpc-timeout"),
                            Ordering::SeqCst,
                        );
                        tokio::time::sleep(Duration::from_secs(1)).await;
                        Ok(Response::new(greet::GreetResponse::default()))
                    }
                    name => {
                        let mut response = Response::new(greet::GreetResponse {
                            message: format!("HTTP {name}"),
                            ..Default::default()
                        });
                        response
                            .metadata_mut()
                            .insert("x-backend-id", "http-backend-6".parse().unwrap());
                        Ok(response)
                    }
                }
            }
        }));
    let (backend_url, backend_task) = start_http(backend).await;
    let http_client = reqwest::Client::builder().build().unwrap();

    let proxy = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    proxy.set_max_unary_response_bytes(256).unwrap();
    proxy
        .connect_http(&http_client, backend_url.clone())
        .unwrap();
    assert_eq!(proxy.tool_catalog().len(), 2);
    let (proxy_address, proxy_task) = serve_native(proxy).await;
    let mut client = generated_client(proxy_address).await;

    let mut request = Request::new(greet::GreetRequest {
        name: "ok".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-request-id", "request-4".parse().unwrap());
    request.metadata_mut().insert(
        "traceparent",
        "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
            .parse()
            .unwrap(),
    );
    request
        .metadata_mut()
        .insert("authorization", "Bearer untrusted".parse().unwrap());
    request
        .metadata_mut()
        .insert("x-tenant", "untrusted-tenant".parse().unwrap());
    request
        .metadata_mut()
        .insert("x-role", "admin".parse().unwrap());
    let response = client.greet(request).await.unwrap();
    assert_eq!(response.get_ref().message, "HTTP ok");
    assert_eq!(
        response.metadata().get("x-backend-id").unwrap(),
        "http-backend-6"
    );

    let mut request = Request::new(greet::GreetRequest {
        name: "fail".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-request-id", "request-4".parse().unwrap());
    let status = client.greet(request).await.unwrap_err();
    assert_eq!(status.code(), Code::FailedPrecondition);
    assert_eq!(status.metadata().get("x-error-id").unwrap(), "http-error-5");
    assert_eq!(
        status.get_details_bad_request().unwrap().field_violations[0].field,
        "name"
    );

    let mut request = Request::new(greet::GreetRequest {
        name: "large".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-request-id", "request-4".parse().unwrap());
    assert_eq!(
        client.greet(request).await.unwrap_err().code(),
        Code::ResourceExhausted
    );

    let mut request = Request::new(greet::GreetRequest {
        name: "slow".into(),
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("x-request-id", "request-4".parse().unwrap());
    request.set_timeout(Duration::from_millis(100));
    assert!(matches!(
        client.greet(request).await.unwrap_err().code(),
        Code::Cancelled | Code::DeadlineExceeded
    ));
    tokio::time::timeout(Duration::from_secs(1), async {
        while !saw_timeout.load(Ordering::SeqCst) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();

    // The caller-owned client remains available and retains its own pool/TLS policy.
    assert_eq!(
        http_client
            .get(backend_url.join("healthz").unwrap())
            .send()
            .await
            .unwrap()
            .status(),
        200
    );
    proxy_task.abort();
    backend_task.abort();
}
