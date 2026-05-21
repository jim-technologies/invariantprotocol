//! Streaming RPC + MCP + CLI tests — full feature parity with Go/Python.

mod common;

use common::{greet, DESCRIPTOR_PATH};
use futures::StreamExt;
use invariant::projections::http::{
    http_router, CONNECT_END_STREAM_FLAG, CONNECT_STREAM_JSON, CONNECT_STREAM_PROTO,
};
use invariant::{Code, Server, ServerStreamTx, Status};
use prost::Message;
use prost_reflect::DynamicMessage;
use std::sync::Arc;

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

async fn err_stream(
    req: greet::StreamGreetRequest,
    tx: ServerStreamTx<greet::GreetResponse>,
) -> Result<(), Status> {
    tx.send(greet::GreetResponse {
        message: format!("first {}", req.name),
        ..Default::default()
    })
    .await?;
    Err(Status::failed_precondition("kapow"))
}

fn build_server() -> Arc<Server> {
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).expect("descriptor");
    srv.register_stream("GreetService.StreamGreet", stream_greet);
    Arc::new(srv)
}

#[tokio::test]
async fn invoke_stream_yields_chunks() {
    let srv = build_server();
    let pool = &srv.parsed().pool;
    let desc = pool
        .get_message_by_name("greet.v1.StreamGreetRequest")
        .unwrap();
    let req = greet::StreamGreetRequest {
        name: "A".into(),
        count: 3,
    };
    let dyn_req = DynamicMessage::decode(desc, &req.encode_to_vec()[..]).unwrap();

    let msgs: Vec<_> = srv
        .invoke_stream("GreetService.StreamGreet", dyn_req)
        .collect()
        .await;
    assert_eq!(msgs.len(), 3);
    for (i, item) in msgs.iter().enumerate() {
        let dyn_msg = item.as_ref().unwrap();
        let raw = dyn_msg.encode_to_vec();
        let typed = greet::GreetResponse::decode(&raw[..]).unwrap();
        assert_eq!(typed.message, format!("Hi A #{i}"));
    }
}

#[tokio::test]
async fn invoke_stream_rejects_unary_tool() {
    let srv = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    srv.register_unary("GreetService.Greet", |_req: greet::GreetRequest| async {
        Ok::<_, Status>(greet::GreetResponse::default())
    });
    let pool = &srv.parsed().pool;
    let desc = pool.get_message_by_name("greet.v1.GreetRequest").unwrap();
    let dyn_req = DynamicMessage::new(desc);

    let items: Vec<_> = srv
        .invoke_stream("GreetService.Greet", dyn_req)
        .collect()
        .await;
    assert_eq!(items.len(), 1);
    let err = items[0].as_ref().unwrap_err();
    assert_eq!(err.code, Code::FailedPrecondition);
}

#[tokio::test]
async fn invoke_unary_rejects_streaming_tool() {
    let srv = build_server();
    let pool = &srv.parsed().pool;
    let desc = pool
        .get_message_by_name("greet.v1.StreamGreetRequest")
        .unwrap();
    let dyn_req = DynamicMessage::new(desc);
    let err = srv
        .invoke("GreetService.StreamGreet", dyn_req)
        .await
        .unwrap_err();
    assert_eq!(err.code, Code::FailedPrecondition);
}

// ---------- HTTP / Connect streaming ----------

async fn start_server(srv: Arc<Server>) -> (String, tokio::task::JoinHandle<()>) {
    let app = http_router(srv);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    tokio::task::yield_now().await;
    (format!("http://{addr}"), handle)
}

fn pack_envelope(flags: u8, payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(5 + payload.len());
    out.push(flags);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

fn read_frames(data: &[u8]) -> Vec<(u8, Vec<u8>)> {
    let mut out = Vec::new();
    let mut i = 0;
    while i < data.len() {
        let flags = data[i];
        let size =
            u32::from_be_bytes([data[i + 1], data[i + 2], data[i + 3], data[i + 4]]) as usize;
        out.push((flags, data[i + 5..i + 5 + size].to_vec()));
        i += 5 + size;
        if flags & CONNECT_END_STREAM_FLAG != 0 {
            break;
        }
    }
    out
}

#[tokio::test]
async fn http_streaming_json_envelopes() {
    let (url, handle) = start_server(build_server()).await;
    let body = pack_envelope(0, br#"{"name":"K","count":3}"#);
    let resp = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", CONNECT_STREAM_JSON)
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    assert_eq!(resp.headers()["content-type"], CONNECT_STREAM_JSON);
    let bytes = resp.bytes().await.unwrap();
    let frames = read_frames(&bytes);
    assert_eq!(frames.len(), 4); // 3 messages + end-stream
    for (i, (flags, payload)) in frames.iter().take(3).enumerate() {
        assert_eq!(*flags, 0);
        let v: serde_json::Value = serde_json::from_slice(payload).unwrap();
        assert_eq!(v["message"], format!("Hi K #{i}"));
    }
    let (end_flags, end_payload) = &frames[3];
    assert_eq!(*end_flags, CONNECT_END_STREAM_FLAG);
    let end: serde_json::Value = serde_json::from_slice(end_payload).unwrap();
    assert!(end.get("error").is_none());
    handle.abort();
}

#[tokio::test]
async fn http_streaming_proto_envelopes() {
    let (url, handle) = start_server(build_server()).await;
    let req = greet::StreamGreetRequest {
        name: "Bin".into(),
        count: 2,
    };
    let body = pack_envelope(0, &req.encode_to_vec());
    let resp = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", CONNECT_STREAM_PROTO)
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.headers()["content-type"], CONNECT_STREAM_PROTO);
    let frames = read_frames(&resp.bytes().await.unwrap());
    assert_eq!(frames.len(), 3); // 2 messages + end
    for (i, (flags, payload)) in frames.iter().take(2).enumerate() {
        assert_eq!(*flags, 0);
        let typed = greet::GreetResponse::decode(&payload[..]).unwrap();
        assert_eq!(typed.message, format!("Hi Bin #{i}"));
    }
    handle.abort();
}

#[tokio::test]
async fn http_streaming_error_in_end_envelope() {
    let srv = Server::from_descriptor(DESCRIPTOR_PATH).unwrap();
    srv.register_stream("GreetService.StreamGreet", err_stream);
    let (url, handle) = start_server(Arc::new(srv)).await;
    let body = pack_envelope(0, br#"{"name":"X"}"#);
    let resp = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", CONNECT_STREAM_JSON)
        .body(body)
        .send()
        .await
        .unwrap();
    let frames = read_frames(&resp.bytes().await.unwrap());
    let (end_flags, end_payload) = frames.last().unwrap();
    assert_eq!(*end_flags, CONNECT_END_STREAM_FLAG);
    let end: serde_json::Value = serde_json::from_slice(end_payload).unwrap();
    assert_eq!(end["error"]["code"], "failed_precondition");
    assert!(end["error"]["message"].as_str().unwrap().contains("kapow"));
    handle.abort();
}

#[tokio::test]
async fn http_streaming_rejects_plain_json_ct() {
    let (url, handle) = start_server(build_server()).await;
    let resp = reqwest::Client::new()
        .post(format!("{url}/greet.v1.GreetService/StreamGreet"))
        .header("content-type", "application/json")
        .body(pack_envelope(0, br#"{"name":"X"}"#))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 400);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["code"], "invalid_argument");
    handle.abort();
}

// ---------- MCP HTTP transport ----------

#[tokio::test]
async fn mcp_http_initialize_tools_list_streaming_call() {
    let (url, handle) = start_server(build_server()).await;
    let client = reqwest::Client::new();

    let init: serde_json::Value = client
        .post(format!("{url}/mcp"))
        .json(&serde_json::json!({"jsonrpc": "2.0", "id": 1, "method": "initialize"}))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    assert_eq!(init["result"]["protocolVersion"], "2024-11-05");

    let list: serde_json::Value = client
        .post(format!("{url}/mcp"))
        .json(&serde_json::json!({"jsonrpc": "2.0", "id": 2, "method": "tools/list"}))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let tools = list["result"]["tools"].as_array().unwrap();
    let entry = tools
        .iter()
        .find(|t| t["name"] == "GreetService.StreamGreet")
        .unwrap();
    assert_eq!(entry["_meta"]["streaming"], true);

    let call: serde_json::Value = client
        .post(format!("{url}/mcp"))
        .json(&serde_json::json!({
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {
                "name": "GreetService.StreamGreet",
                "arguments": {"name": "Stream", "count": 3},
            }
        }))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let content = call["result"]["content"].as_array().unwrap();
    assert_eq!(content.len(), 3);
    handle.abort();
}

#[tokio::test]
async fn mcp_http_notification_returns_204() {
    let (url, handle) = start_server(build_server()).await;
    let resp = reqwest::Client::new()
        .post(format!("{url}/mcp"))
        .json(&serde_json::json!({"jsonrpc": "2.0", "method": "notifications/initialized"}))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 204);
    handle.abort();
}

// ---------- CLI ----------

#[tokio::test]
async fn cli_unary_writes_pretty_json() {
    let srv = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).unwrap());
    srv.register_unary(
        "GreetService.Greet",
        |req: greet::GreetRequest| async move {
            Ok::<_, Status>(greet::GreetResponse {
                message: format!("Hi {}", req.name),
                ..Default::default()
            })
        },
    );
    let mut out = Vec::new();
    invariant::projections::cli::cli_write(
        srv,
        &[
            "GreetService".into(),
            "Greet".into(),
            "-r".into(),
            r#"{"name":"Z"}"#.into(),
        ],
        &mut out,
    )
    .await
    .unwrap();
    let parsed: serde_json::Value = serde_json::from_slice(&out).unwrap();
    assert_eq!(parsed["message"], "Hi Z");
}

#[tokio::test]
async fn cli_stream_writes_ndjson() {
    let mut out = Vec::new();
    invariant::projections::cli::cli_write(
        build_server(),
        &[
            "GreetService".into(),
            "StreamGreet".into(),
            "-r".into(),
            r#"{"name":"Z","count":2}"#.into(),
        ],
        &mut out,
    )
    .await
    .unwrap();
    let text = String::from_utf8(out).unwrap();
    let lines: Vec<&str> = text.lines().collect();
    assert_eq!(lines.len(), 2);
    let chunk0: serde_json::Value = serde_json::from_str(lines[0]).unwrap();
    let chunk1: serde_json::Value = serde_json::from_str(lines[1]).unwrap();
    assert_eq!(chunk0["message"], "Hi Z #0");
    assert_eq!(chunk1["message"], "Hi Z #1");
}

#[tokio::test]
async fn stream_interceptor_chain_wraps_call() {
    use std::sync::atomic::{AtomicU32, Ordering};
    let srv = build_server();
    let saw = Arc::new(AtomicU32::new(0));
    let saw_clone = saw.clone();
    srv.use_stream_interceptor(Arc::new(move |req, tx, info, next| {
        let saw = saw_clone.clone();
        Box::pin(async move {
            saw.fetch_add(1, Ordering::SeqCst);
            assert_eq!(info.full_method, "/greet.v1.GreetService/StreamGreet");
            next(req, tx).await
        })
    }));

    let pool = &srv.parsed().pool;
    let desc = pool
        .get_message_by_name("greet.v1.StreamGreetRequest")
        .unwrap();
    let req = greet::StreamGreetRequest {
        name: "X".into(),
        count: 2,
    };
    let dyn_req = DynamicMessage::decode(desc, &req.encode_to_vec()[..]).unwrap();

    let msgs: Vec<_> = srv
        .invoke_stream("GreetService.StreamGreet", dyn_req)
        .collect()
        .await;
    assert_eq!(msgs.len(), 2);
    assert_eq!(saw.load(Ordering::SeqCst), 1);
}
