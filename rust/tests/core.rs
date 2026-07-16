//! Descriptor, generated registration, and in-process projection dispatch.

mod common;

use common::{DESCRIPTOR_PATH, TestGreetService, greet, registered_server};
use invariant::{Code, ErasedRequest, Request, Response, Server, Status};
use prost::Message;
use prost_reflect::DynamicMessage;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

#[test]
fn descriptor_parsing_preserves_services_messages_comments_and_cardinality() {
    let parsed = invariant::ParsedDescriptor::from_file(DESCRIPTOR_PATH).unwrap();
    let service = &parsed.services["greet.v1.GreetService"];
    assert_eq!(service.name, "GreetService");
    assert!(service.comment.contains("simple greeting service"));
    assert_eq!(
        service
            .methods
            .keys()
            .map(String::as_str)
            .collect::<Vec<_>>(),
        ["Greet", "GreetGroup", "StreamGreet"]
    );
    assert!(service.methods["StreamGreet"].server_streaming);
    assert!(!service.methods["StreamGreet"].client_streaming);
    assert!(parsed.messages.contains_key("greet.v1.GreetRequest"));
    assert_eq!(
        parsed.enums["greet.v1.Mood"]
            .values
            .iter()
            .map(|value| value.name.as_str())
            .collect::<Vec<_>>(),
        ["MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"]
    );
}

#[test]
fn generated_registration_populates_the_projection_catalog() {
    let server = registered_server(TestGreetService::default());
    let catalog = server.tool_catalog();
    assert_eq!(catalog.len(), 3);
    assert_eq!(catalog[0]["name"], "GreetService.Greet");
    assert!(
        catalog[0]["description"]
            .as_str()
            .unwrap()
            .contains("Greet a person")
    );
    assert_eq!(catalog[2]["_meta"]["streaming"], true);
}

#[tokio::test]
async fn in_process_projection_uses_registered_typed_implementation_and_metadata() {
    let calls = Arc::new(AtomicUsize::new(0));
    let handler_calls = calls.clone();
    let service = TestGreetService::default().with_greet(move |request| {
        let handler_calls = handler_calls.clone();
        async move {
            handler_calls.fetch_add(1, Ordering::SeqCst);
            assert_eq!(request.metadata().get("x-correlation-id").unwrap(), "abc");
            let mut response = Response::new(greet::GreetResponse {
                message: format!("Hello {}", request.get_ref().name),
                ..Default::default()
            });
            response
                .metadata_mut()
                .insert("x-result-id", "result-1".parse().unwrap());
            Ok(response)
        }
    });
    let server = registered_server(service);

    let interceptor_calls = Arc::new(AtomicUsize::new(0));
    let seen = interceptor_calls.clone();
    server
        .use_shared_unary(Arc::new(move |request: ErasedRequest, info, next| {
            let seen = seen.clone();
            Box::pin(async move {
                seen.fetch_add(1, Ordering::SeqCst);
                assert_eq!(info.full_method, "/greet.v1.GreetService/Greet");
                assert_eq!(
                    request
                        .downcast_ref::<greet::GreetRequest>()
                        .unwrap()
                        .get_ref()
                        .name,
                    "Projection"
                );
                next(request).await
            })
        }))
        .unwrap();

    let descriptor = server
        .parsed()
        .pool
        .get_message_by_name("greet.v1.GreetRequest")
        .unwrap();
    let request = greet::GreetRequest {
        name: "Projection".into(),
        ..Default::default()
    };
    let dynamic = DynamicMessage::decode(descriptor, request.encode_to_vec().as_slice()).unwrap();
    let mut request = Request::new(dynamic);
    request
        .metadata_mut()
        .insert("x-correlation-id", "abc".parse().unwrap());
    let response = server.invoke("GreetService.Greet", request).await.unwrap();
    assert_eq!(response.metadata().get("x-result-id").unwrap(), "result-1");
    let typed =
        greet::GreetResponse::decode(response.into_inner().encode_to_vec().as_slice()).unwrap();
    assert_eq!(typed.message, "Hello Projection");
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(interceptor_calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn invocation_rejects_unknown_tools_and_wrong_cardinality() {
    let server = registered_server(TestGreetService::default());
    let request_descriptor = server
        .parsed()
        .pool
        .get_message_by_name("greet.v1.GreetRequest")
        .unwrap();
    let request = DynamicMessage::new(request_descriptor);
    assert_eq!(
        server
            .invoke("missing", Request::new(request.clone()))
            .await
            .unwrap_err()
            .code(),
        Code::NotFound
    );
    let status = match server
        .invoke_stream("GreetService.Greet", Request::new(request))
        .await
    {
        Ok(_) => panic!("unary method unexpectedly returned a stream"),
        Err(status) => status,
    };
    assert_eq!(status.code(), Code::FailedPrecondition);
}

#[tokio::test]
async fn invocation_freezes_configuration_deterministically() {
    let server = registered_server(TestGreetService::default());
    let descriptor = server
        .parsed()
        .pool
        .get_message_by_name("greet.v1.GreetRequest")
        .unwrap();
    server
        .invoke(
            "GreetService.Greet",
            Request::new(DynamicMessage::new(descriptor)),
        )
        .await
        .unwrap();
    let noop =
        Arc::new(|request: ErasedRequest, _info, next: invariant::SharedHandler| next(request));
    assert_eq!(
        server.use_shared_unary(noop).unwrap_err().code(),
        Code::FailedPrecondition
    );
    assert_eq!(
        server.set_max_unary_request_bytes(1).unwrap_err().code(),
        Code::FailedPrecondition
    );
}

#[test]
fn generated_service_trait_is_the_application_contract() {
    fn assert_service<T: greet::greet_service_server::GreetService>() {}
    assert_service::<TestGreetService>();

    let _: fn(&Server, TestGreetService) -> Result<(), Status> =
        greet::register_greet_service_server;
}
