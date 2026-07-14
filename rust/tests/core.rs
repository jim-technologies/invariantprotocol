//! Core dispatch tests — descriptor parsing, registration, invoke, catalog.
//! Mirrors `go/descriptor_test.go` + `go/server_test.go` shape.

mod common;

use common::{DESCRIPTOR_PATH, greet};
use invariant::{Code, Server, Status};
use prost::Message;
use prost_reflect::DynamicMessage;

fn build_server() -> Server {
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).expect("load descriptor");
    srv.register_unary("GreetService.Greet", greet);
    srv
}

async fn greet(req: greet::GreetRequest) -> Result<greet::GreetResponse, Status> {
    Ok(greet::GreetResponse {
        message: format!("Hi {}", req.name),
        ..Default::default()
    })
}

async fn greet_group(req: greet::GreetGroupRequest) -> Result<greet::GreetGroupResponse, Status> {
    let messages: Vec<String> = req
        .people
        .iter()
        .map(|p| format!("Hi {}", p.name))
        .collect();
    let count = messages.len() as i32;
    Ok(greet::GreetGroupResponse { messages, count })
}

#[test]
fn parse_descriptor_extracts_services() {
    let parsed = invariant::ParsedDescriptor::from_file(DESCRIPTOR_PATH).expect("load");
    let svc = parsed
        .services
        .get("greet.v1.GreetService")
        .expect("greet service");
    assert_eq!(svc.name, "GreetService");
    assert!(
        svc.comment
            .to_lowercase()
            .contains("simple greeting service"),
        "expected comment, got {:?}",
        svc.comment
    );
    assert!(svc.methods.contains_key("Greet"));
    assert!(svc.methods.contains_key("GreetGroup"));
    assert!(svc.methods.contains_key("StreamGreet"));

    let stream = &svc.methods["StreamGreet"];
    assert!(stream.server_streaming);
    assert!(!stream.client_streaming);
}

#[test]
fn parse_descriptor_extracts_messages_and_enums() {
    let parsed = invariant::ParsedDescriptor::from_file(DESCRIPTOR_PATH).expect("load");
    assert!(parsed.messages.contains_key("greet.v1.GreetRequest"));
    assert!(parsed.messages.contains_key("greet.v1.GreetResponse"));
    let mood = parsed.enums.get("greet.v1.Mood").expect("mood enum");
    let names: Vec<&str> = mood.values.iter().map(|v| v.name.as_str()).collect();
    assert_eq!(names, vec!["MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"]);
}

#[tokio::test]
async fn invoke_unary_dispatches_to_handler() {
    let srv = build_server();
    let pool = &srv.parsed().pool;
    let desc = pool.get_message_by_name("greet.v1.GreetRequest").unwrap();
    let typed = greet::GreetRequest {
        name: "World".into(),
        ..Default::default()
    };
    let buf = typed.encode_to_vec();
    let dyn_req = DynamicMessage::decode(desc, &buf[..]).unwrap();

    let dyn_resp = srv
        .invoke("GreetService.Greet", dyn_req)
        .await
        .expect("invoke");
    let raw = dyn_resp.encode_to_vec();
    let resp = greet::GreetResponse::decode(&raw[..]).unwrap();
    assert_eq!(resp.message, "Hi World");
}

#[tokio::test]
async fn invoke_unknown_tool_returns_not_found() {
    let srv = build_server();
    let pool = &srv.parsed().pool;
    let desc = pool.get_message_by_name("greet.v1.GreetRequest").unwrap();
    let dyn_req = DynamicMessage::new(desc);
    let err = srv
        .invoke("does.not.Exist", dyn_req)
        .await
        .expect_err("must fail");
    assert_eq!(err.code, Code::NotFound);
}

#[test]
fn tool_catalog_lists_registered_tools() {
    let srv = build_server();
    srv.register_unary("GreetService.GreetGroup", greet_group);
    let catalog = srv.tool_catalog();
    assert_eq!(catalog.len(), 2);

    let names: Vec<&str> = catalog
        .iter()
        .map(|e| e["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, vec!["GreetService.Greet", "GreetService.GreetGroup"]);

    // Description picked up from proto leading comments.
    let greet_entry = catalog
        .iter()
        .find(|e| e["name"] == "GreetService.Greet")
        .unwrap();
    assert!(
        greet_entry["description"]
            .as_str()
            .unwrap()
            .to_lowercase()
            .contains("greet a person")
    );

    // No `_meta.streaming` on unary tools.
    assert!(greet_entry.get("_meta").is_none());
}

#[tokio::test]
async fn interceptor_chain_runs_outer_then_inner() {
    use std::sync::Arc as StdArc;
    use std::sync::atomic::{AtomicU32, Ordering};

    let srv = build_server();
    let outer = StdArc::new(AtomicU32::new(0));
    let inner = StdArc::new(AtomicU32::new(0));
    let outer_clone = outer.clone();
    let inner_clone = inner.clone();

    srv.use_interceptor(StdArc::new(move |req, info, next| {
        let outer = outer_clone.clone();
        Box::pin(async move {
            outer.fetch_add(1, Ordering::SeqCst);
            assert_eq!(info.full_method, "/greet.v1.GreetService/Greet");
            next(req).await
        })
    }));
    srv.use_interceptor(StdArc::new(move |req, _info, next| {
        let inner = inner_clone.clone();
        Box::pin(async move {
            inner.fetch_add(1, Ordering::SeqCst);
            next(req).await
        })
    }));

    let pool = &srv.parsed().pool;
    let desc = pool.get_message_by_name("greet.v1.GreetRequest").unwrap();
    let typed = greet::GreetRequest {
        name: "A".into(),
        ..Default::default()
    };
    let dyn_req = DynamicMessage::decode(desc, &typed.encode_to_vec()[..]).unwrap();
    srv.invoke("GreetService.Greet", dyn_req).await.unwrap();

    assert_eq!(outer.load(Ordering::SeqCst), 1);
    assert_eq!(inner.load(Ordering::SeqCst), 1);
}
