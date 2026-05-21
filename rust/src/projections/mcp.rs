//! MCP (Model Context Protocol) projection.
//!
//! Two transports:
//! - `serve_mcp_stdio`: line-delimited JSON-RPC over stdin/stdout
//! - `mcp_dispatch`: one JSON-RPC message in → one JSON-RPC response out,
//!   used by the HTTP projection's `/mcp` route
//!
//! Streaming tools collect each emitted chunk into the response `content`
//! array — same opinionated cut as Go/Python (no progress notifications).

use crate::errors::Status;
use crate::server::Server;
use parking_lot::Mutex;
use prost_reflect::{DynamicMessage, SerializeOptions};
use serde_json::{json, Value};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::sync::Mutex as AsyncMutex;
use tokio::task::JoinHandle;

pub const MCP_PROTOCOL_VERSION: &str = "2024-11-05";

/// Run the stdio MCP transport. Blocks until stdin closes.
///
/// `tools/call` runs concurrently in spawned tasks so a `notifications/cancelled`
/// arriving on the next line can interrupt a long-running tool. Fast metadata
/// methods (`initialize`, `tools/list`, `ping`) run inline to keep response
/// order deterministic. Mirrors Go's `mcpSession.run` and Python's `_StdioMCP`.
pub async fn serve_mcp_stdio(server: Arc<Server>) -> std::io::Result<()> {
    let stdin = tokio::io::stdin();
    let mut reader = BufReader::new(stdin).lines();
    let stdout = Arc::new(AsyncMutex::new(tokio::io::stdout()));
    let inflight: Arc<Mutex<HashMap<String, JoinHandle<()>>>> = Arc::new(Mutex::new(HashMap::new()));

    while let Some(line) = reader.next_line().await? {
        if line.trim().is_empty() {
            continue;
        }
        let msg: Value = match serde_json::from_str(&line) {
            Ok(v) => v,
            Err(e) => {
                write_response(
                    &stdout,
                    &json!({
                        "jsonrpc": "2.0",
                        "id": null,
                        "error": {"code": -32700, "message": format!("Parse error: {e}")},
                    }),
                )
                .await?;
                continue;
            }
        };

        // Notifications run inline so cancellation takes effect before the
        // next message is read.
        if msg.get("id").map_or(true, |v| v.is_null()) {
            handle_notification(&msg, &inflight);
            continue;
        }

        // `tools/call` is the only method that can block on user code —
        // dispatch concurrently so notifications can interrupt it. Other
        // metadata methods run inline to keep ordering deterministic.
        if msg.get("method").and_then(|v| v.as_str()) != Some("tools/call") {
            if let Some(resp) = mcp_dispatch(&server, &msg).await {
                write_response(&stdout, &resp).await?;
            }
            continue;
        }

        let id_key = id_key(msg.get("id").cloned().unwrap_or(Value::Null));
        let server_clone = server.clone();
        let stdout_clone = stdout.clone();
        let inflight_clone = inflight.clone();
        let id_key_for_done = id_key.clone();
        let task = tokio::spawn(async move {
            if let Some(resp) = mcp_dispatch(&server_clone, &msg).await {
                let _ = write_response(&stdout_clone, &resp).await;
            }
            inflight_clone.lock().remove(&id_key_for_done);
        });
        inflight.lock().insert(id_key, task);
    }

    // On stdin EOF wait for in-flight tools/call tasks to finish so their
    // responses still reach the client (matches Go's `wg.Wait()`).
    let pending: Vec<JoinHandle<()>> = inflight.lock().drain().map(|(_, h)| h).collect();
    for h in pending {
        let _ = h.await;
    }
    Ok(())
}

async fn write_response(
    stdout: &Arc<AsyncMutex<tokio::io::Stdout>>,
    resp: &Value,
) -> std::io::Result<()> {
    let mut body = serde_json::to_vec(resp).unwrap_or_default();
    body.push(b'\n');
    let mut g = stdout.lock().await;
    g.write_all(&body).await?;
    g.flush().await
}

fn handle_notification(msg: &Value, inflight: &Arc<Mutex<HashMap<String, JoinHandle<()>>>>) {
    if msg.get("method").and_then(|v| v.as_str()) != Some("notifications/cancelled") {
        return;
    }
    let Some(request_id) = msg.get("params").and_then(|p| p.get("requestId")).cloned() else {
        return;
    };
    let key = id_key(request_id);
    if let Some(task) = inflight.lock().remove(&key) {
        task.abort();
    }
}

fn id_key(id: Value) -> String {
    // JSON-RPC ids may be string or number; normalize for map keys.
    format!("{}:{id}", id_type(&id))
}

fn id_type(id: &Value) -> &'static str {
    match id {
        Value::Number(_) => "num",
        Value::String(_) => "str",
        Value::Null => "null",
        _ => "other",
    }
}

/// Dispatch a single MCP JSON-RPC message. Returns `None` for notifications
/// (no `id` field); otherwise a fully-formed JSON-RPC response.
pub async fn mcp_dispatch(server: &Arc<Server>, msg: &Value) -> Option<Value> {
    let method = msg.get("method")?.as_str()?;
    let id = msg.get("id").cloned();
    if id.is_none() || id.as_ref() == Some(&Value::Null) {
        return None;
    }
    let id = id.unwrap();
    let params = msg.get("params").cloned().unwrap_or(Value::Null);

    match method {
        "initialize" => Some(json!({
            "jsonrpc": "2.0",
            "id": id,
            "result": {
                "protocolVersion": MCP_PROTOCOL_VERSION,
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "invariant-protocol", "version": env!("CARGO_PKG_VERSION")},
            }
        })),
        "tools/list" => Some(json!({
            "jsonrpc": "2.0",
            "id": id,
            "result": {"tools": server.tool_catalog()},
        })),
        "tools/call" => Some(tools_call(server, id, &params).await),
        "ping" => Some(json!({"jsonrpc": "2.0", "id": id, "result": {}})),
        _ => Some(json!({
            "jsonrpc": "2.0",
            "id": id,
            "error": {"code": -32601, "message": format!("Method not found: {method}")},
        })),
    }
}

async fn tools_call(server: &Arc<Server>, id: Value, params: &Value) -> Value {
    let tool_name = params.get("name").and_then(|v| v.as_str()).unwrap_or("");
    let arguments = params.get("arguments").cloned().unwrap_or(json!({}));
    let Some(tool) = server.tool(tool_name) else {
        return json!({
            "jsonrpc": "2.0",
            "id": id,
            "error": {"code": -32602, "message": format!("Unknown tool: {tool_name}")},
        });
    };

    let request = match build_dyn_request(&tool, &arguments) {
        Ok(r) => r,
        Err(s) => return error_result(id, &s),
    };

    if tool.server_streaming {
        return stream_tools_call(server, id, &tool.name, request).await;
    }

    match server.invoke(tool_name, request).await {
        Ok(resp) => {
            let text = serialize_message(&resp);
            json!({
                "jsonrpc": "2.0",
                "id": id,
                "result": {"content": [{"type": "text", "text": text}]},
            })
        }
        Err(s) => error_result(id, &s),
    }
}

async fn stream_tools_call(
    server: &Arc<Server>,
    id: Value,
    tool_name: &str,
    request: DynamicMessage,
) -> Value {
    use futures::StreamExt;
    let mut stream = server.invoke_stream(tool_name, request);
    let mut content: Vec<Value> = Vec::new();
    while let Some(item) = stream.next().await {
        match item {
            Ok(msg) => {
                let text = serialize_message(&msg);
                content.push(json!({"type": "text", "text": text}));
            }
            Err(s) => {
                content.push(json!({"type": "text", "text": s.message.clone()}));
                return json!({
                    "jsonrpc": "2.0",
                    "id": id,
                    "result": {
                        "content": content,
                        "isError": true,
                        "error": s.to_payload(),
                    },
                });
            }
        }
    }
    json!({
        "jsonrpc": "2.0",
        "id": id,
        "result": {"content": content},
    })
}

fn build_dyn_request(
    tool: &Arc<crate::server::Tool>,
    arguments: &Value,
) -> Result<DynamicMessage, Status> {
    let bytes = serde_json::to_vec(arguments).unwrap_or_default();
    if bytes.is_empty() || bytes == b"null" {
        return Ok(DynamicMessage::new(tool.input_desc.clone()));
    }
    let mut deserializer = serde_json::Deserializer::from_slice(&bytes);
    let opts = prost_reflect::DeserializeOptions::new();
    DynamicMessage::deserialize_with_options(tool.input_desc.clone(), &mut deserializer, &opts)
        .map_err(|e| Status::invalid_argument(format!("proto: {e}")))
}

fn serialize_message(msg: &DynamicMessage) -> String {
    let opts = SerializeOptions::new().use_proto_field_name(true);
    let mut buf = Vec::with_capacity(128);
    let mut ser = serde_json::Serializer::pretty(&mut buf);
    let _ = msg.serialize_with_options(&mut ser, &opts);
    String::from_utf8(buf).unwrap_or_default()
}

fn error_result(id: Value, err: &Status) -> Value {
    json!({
        "jsonrpc": "2.0",
        "id": id,
        "result": {
            "content": [{"type": "text", "text": err.message.clone()}],
            "isError": true,
            "error": err.to_payload(),
        },
    })
}
