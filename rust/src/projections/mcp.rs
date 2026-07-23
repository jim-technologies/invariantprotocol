//! MCP (Model Context Protocol) projection.
//!
//! Two transports:
//! - `serve_mcp_stdio`: line-delimited JSON-RPC over stdin/stdout
//! - `mcp_dispatch`: one JSON-RPC message in → one JSON-RPC response out,
//!   used by the HTTP projection's `/mcp` route
//!
//! Streaming tools collect each emitted chunk into the response `content`
//! array — same opinionated cut as Go/Python (no progress notifications).

use crate::errors::error_payload;
use crate::server::{ProjectionContext, Server};
use parking_lot::Mutex;
use prost_reflect::{DynamicMessage, SerializeOptions};
use serde_json::{Value, json};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};
use tokio::sync::Mutex as AsyncMutex;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;
use tonic::metadata::MetadataMap;
use tonic::{Request, Status};

pub const MCP_PROTOCOL_VERSION: &str = "2025-11-25";
const JSONRPC_MAX_SAFE_INTEGER: i64 = 9_007_199_254_740_991;

/// Run the stdio MCP transport. Blocks until stdin closes.
///
/// `tools/call` runs concurrently in spawned tasks so a `notifications/cancelled`
/// arriving on the next line can interrupt a long-running tool. Fast metadata
/// methods (`initialize`, `tools/list`, `ping`) run inline to keep response
/// order deterministic. Mirrors Go's `mcpSession.run` and Python's `_StdioMCP`.
pub async fn serve_mcp_stdio(server: Arc<Server>) -> std::io::Result<()> {
    serve_mcp_session(
        server,
        tokio::io::stdin(),
        tokio::io::stdout(),
        CancellationToken::new(),
    )
    .await
}

pub(crate) async fn serve_mcp_stdio_until_cancelled(
    server: Arc<Server>,
    shutdown: CancellationToken,
) -> std::io::Result<()> {
    serve_mcp_session(server, tokio::io::stdin(), tokio::io::stdout(), shutdown).await
}

async fn serve_mcp_session<R, W>(
    server: Arc<Server>,
    input: R,
    output: W,
    shutdown: CancellationToken,
) -> std::io::Result<()>
where
    R: AsyncRead + Unpin,
    W: AsyncWrite + Unpin + Send + 'static,
{
    server.freeze();
    let mut reader = BufReader::new(input);
    let stdout = Arc::new(AsyncMutex::new(output));
    let inflight: Arc<Mutex<HashMap<String, JoinHandle<()>>>> =
        Arc::new(Mutex::new(HashMap::new()));

    let mut line = Vec::new();
    let shutdown_requested = loop {
        line.clear();
        let bytes_read = tokio::select! {
            biased;
            _ = shutdown.cancelled() => break true,
            result = reader.read_until(b'\n', &mut line) => result?,
        };
        if bytes_read == 0 {
            break false;
        }
        if line.last() == Some(&b'\n') {
            line.pop();
        }
        if line.last() == Some(&b'\r') {
            line.pop();
        }
        if line.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        let msg: Value = match serde_json::from_slice(&line) {
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
        if let Some(response) = invalid_request_response(&msg) {
            write_response(&stdout, &response).await?;
            continue;
        }
        if is_client_response(&msg) {
            continue;
        }

        // Notifications run inline so cancellation takes effect before the
        // next message is read.
        if msg.get("id").is_none() {
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
        let (registered, registration) = tokio::sync::oneshot::channel();
        let task = tokio::spawn(async move {
            // A multi-threaded runtime may poll this task immediately. Wait
            // until its handle is registered so fast calls cannot remove a
            // not-yet-present entry and leave a completed handle behind.
            let _ = registration.await;
            if let Some(resp) = mcp_dispatch(&server_clone, &msg).await {
                let _ = write_response(&stdout_clone, &resp).await;
            }
            inflight_clone.lock().remove(&id_key_for_done);
        });
        inflight.lock().insert(id_key, task);
        let _ = registered.send(());
    };

    // On stdin EOF wait for in-flight tools/call tasks to finish so their
    // responses still reach the client (matches Go's `wg.Wait()`). Cooperative
    // projection shutdown instead aborts and joins them before returning.
    let mut pending: Vec<JoinHandle<()>> = inflight.lock().drain().map(|(_, h)| h).collect();
    if shutdown_requested {
        for task in &pending {
            task.abort();
        }
    }
    while let Some(mut task) = pending.pop() {
        if shutdown_requested {
            let _ = task.await;
            continue;
        }
        tokio::select! {
            _ = shutdown.cancelled() => {
                task.abort();
                for remaining in &pending {
                    remaining.abort();
                }
                let _ = task.await;
                for remaining in pending {
                    let _ = remaining.await;
                }
                return Ok(());
            }
            _ = &mut task => {}
        }
    }
    Ok(())
}

async fn write_response<W: AsyncWrite + Unpin>(
    stdout: &Arc<AsyncMutex<W>>,
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
    let id = canonical_jsonrpc_id(id);
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
    mcp_dispatch_with_context(server, msg, MetadataMap::new(), None, None)
        .await
        .expect("unbounded MCP dispatch cannot exceed an HTTP response limit")
}

pub(crate) async fn mcp_dispatch_with_context(
    server: &Arc<Server>,
    msg: &Value,
    metadata: MetadataMap,
    projection: Option<ProjectionContext>,
    max_response_bytes: Option<usize>,
) -> Result<Option<Value>, Status> {
    server.freeze();
    if let Some(response) = invalid_request_response(msg) {
        return Ok(Some(response));
    }
    if is_client_response(msg) {
        return Ok(None);
    }
    let method = msg
        .get("method")
        .and_then(Value::as_str)
        .expect("validated JSON-RPC method");
    let Some(id) = msg.get("id").cloned().map(canonical_jsonrpc_id) else {
        return Ok(None);
    };
    let params = msg.get("params").cloned().unwrap_or_else(|| json!({}));
    if !params.is_object() {
        return Ok(Some(invalid_params(id)));
    }

    let response = match method {
        "initialize" => {
            if !valid_initialize_params(&params) {
                return Ok(Some(invalid_params(id)));
            }
            json!({
                "jsonrpc": "2.0",
                "id": id,
                "result": {
                    "protocolVersion": MCP_PROTOCOL_VERSION,
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": "invariant-protocol", "version": env!("CARGO_PKG_VERSION")},
                }
            })
        }
        "tools/list" => json!({
            "jsonrpc": "2.0",
            "id": id,
            "result": {"tools": server.tool_catalog()},
        }),
        "tools/call" => {
            if !params.get("name").is_some_and(Value::is_string)
                || params
                    .get("arguments")
                    .is_some_and(|arguments| !arguments.is_object())
            {
                return Ok(Some(invalid_params(id)));
            }
            tools_call(
                server,
                id,
                &params,
                metadata,
                projection,
                max_response_bytes,
            )
            .await?
        }
        "ping" => json!({"jsonrpc": "2.0", "id": id, "result": {}}),
        _ => json!({
            "jsonrpc": "2.0",
            "id": id,
            "error": {"code": -32601, "message": format!("Method not found: {method}")},
        }),
    };
    Ok(Some(response))
}

fn valid_initialize_params(params: &Value) -> bool {
    let Some(params) = params.as_object() else {
        return false;
    };
    let Some(client_info) = params.get("clientInfo").and_then(Value::as_object) else {
        return false;
    };
    params.get("protocolVersion").is_some_and(Value::is_string)
        && params.get("capabilities").is_some_and(Value::is_object)
        && client_info.get("name").is_some_and(Value::is_string)
        && client_info.get("version").is_some_and(Value::is_string)
}

pub(crate) fn invalid_request_response(msg: &Value) -> Option<Value> {
    let Some(object) = msg.as_object() else {
        return Some(invalid_request(Value::Null));
    };
    if is_client_response(msg) {
        return None;
    }
    if object.get("jsonrpc").and_then(Value::as_str) != Some("2.0")
        || !object.get("method").is_some_and(Value::is_string)
        || object.get("id").is_some_and(|id| !valid_jsonrpc_id(id))
    {
        return Some(invalid_request(Value::Null));
    }
    None
}

pub(crate) fn is_client_response(msg: &Value) -> bool {
    let Some(object) = msg.as_object() else {
        return false;
    };
    if object.get("jsonrpc").and_then(Value::as_str) != Some("2.0") || object.contains_key("method")
    {
        return false;
    }
    match (object.get("result"), object.get("error")) {
        (Some(result), None) => {
            result.is_object() && object.get("id").is_some_and(valid_jsonrpc_id)
        }
        (None, Some(error)) => {
            object.get("id").is_none_or(valid_jsonrpc_id)
                && error.as_object().is_some_and(|error| {
                    error.get("code").is_some_and(valid_jsonrpc_integer)
                        && error.get("message").is_some_and(Value::is_string)
                })
        }
        _ => false,
    }
}

fn valid_jsonrpc_id(id: &Value) -> bool {
    match id {
        Value::String(_) => true,
        Value::Number(_) => jsonrpc_safe_integer(id).is_some(),
        _ => false,
    }
}

fn canonical_jsonrpc_id(id: Value) -> Value {
    match jsonrpc_safe_integer(&id) {
        Some(integer) => Value::Number(integer.into()),
        None => id,
    }
}

fn jsonrpc_safe_integer(value: &Value) -> Option<i64> {
    let number = value.as_number()?;
    if let Some(integer) = number.as_i64() {
        return (-JSONRPC_MAX_SAFE_INTEGER..=JSONRPC_MAX_SAFE_INTEGER)
            .contains(&integer)
            .then_some(integer);
    }
    if let Some(integer) = number.as_u64() {
        return (integer <= JSONRPC_MAX_SAFE_INTEGER as u64).then_some(integer as i64);
    }
    let float = number.as_f64()?;
    (float.is_finite()
        && float.fract() == 0.0
        && float >= -(JSONRPC_MAX_SAFE_INTEGER as f64)
        && float <= JSONRPC_MAX_SAFE_INTEGER as f64)
        .then_some(if float == 0.0 { 0 } else { float as i64 })
}

fn valid_jsonrpc_integer(value: &Value) -> bool {
    value
        .as_number()
        .is_some_and(|number| number.is_i64() || number.is_u64())
}

fn invalid_request(id: Value) -> Value {
    json!({
        "jsonrpc": "2.0",
        "id": id,
        "error": {"code": -32600, "message": "Invalid Request"},
    })
}

fn invalid_params(id: Value) -> Value {
    json!({
        "jsonrpc": "2.0",
        "id": id,
        "error": {"code": -32602, "message": "Invalid params"},
    })
}

async fn tools_call(
    server: &Arc<Server>,
    id: Value,
    params: &Value,
    metadata: MetadataMap,
    projection: Option<ProjectionContext>,
    max_response_bytes: Option<usize>,
) -> Result<Value, Status> {
    let tool_name = params.get("name").and_then(|v| v.as_str()).unwrap_or("");
    let arguments = params.get("arguments").cloned().unwrap_or(json!({}));
    let Some(tool) = server.tool(tool_name) else {
        return Ok(json!({
            "jsonrpc": "2.0",
            "id": id,
            "error": {"code": -32602, "message": format!("Unknown tool: {tool_name}")},
        }));
    };

    let request = match build_dyn_request(&tool, &arguments) {
        Ok(r) => r,
        Err(s) => return Ok(error_result(id, &s)),
    };

    if tool.server_streaming {
        return stream_tools_call(
            server,
            id,
            &tool.name,
            request,
            metadata,
            projection,
            max_response_bytes,
        )
        .await;
    }

    let mut request = Request::new(request);
    *request.metadata_mut() = metadata;
    if let Some(projection) = projection {
        if let Some(remaining) = projection.remaining() {
            request.set_timeout(remaining);
        }
        request.extensions_mut().insert(projection);
    }
    Ok(match server.invoke(tool_name, request).await {
        Ok(resp) => {
            let text = serialize_message(resp.get_ref());
            json!({
                "jsonrpc": "2.0",
                "id": id,
                "result": {"content": [{"type": "text", "text": text}]},
            })
        }
        Err(s) => error_result(id, &s),
    })
}

async fn stream_tools_call(
    server: &Arc<Server>,
    id: Value,
    tool_name: &str,
    request: DynamicMessage,
    metadata: MetadataMap,
    projection: Option<ProjectionContext>,
    max_response_bytes: Option<usize>,
) -> Result<Value, Status> {
    use futures::StreamExt;
    // MCP over stateless HTTP returns one buffered JSON-RPC document, so its
    // existing unary response limit must also bound collection of streaming
    // tool chunks. Direct dispatch and stdio have no HTTP response budget.
    let mut encoded_response_bytes = max_response_bytes.map(|_| {
        serde_json::to_vec(&json!({
            "jsonrpc": "2.0",
            "id": id.clone(),
            "result": {"content": []},
        }))
        .unwrap_or_default()
        .len()
    });
    if max_response_bytes
        .zip(encoded_response_bytes)
        .is_some_and(|(limit, encoded)| encoded > limit)
    {
        return Err(mcp_response_limit_error());
    }
    let mut request = Request::new(request);
    *request.metadata_mut() = metadata;
    if let Some(projection) = projection {
        if let Some(remaining) = projection.remaining() {
            request.set_timeout(remaining);
        }
        request.extensions_mut().insert(projection);
    }
    let mut stream = match server.invoke_stream(tool_name, request).await {
        Ok(response) => response.into_inner(),
        Err(status) => return Ok(error_result(id, &status)),
    };
    let mut content: Vec<Value> = Vec::new();
    while let Some(item) = stream.next().await {
        match item {
            Ok(msg) => {
                let text = serialize_message(&msg);
                let block = json!({"type": "text", "text": text});
                if let (Some(limit), Some(encoded)) =
                    (max_response_bytes, encoded_response_bytes.as_mut())
                {
                    let block_bytes = serde_json::to_vec(&block).unwrap_or_default().len();
                    let next = encoded
                        .saturating_add(usize::from(!content.is_empty()))
                        .saturating_add(block_bytes);
                    if next > limit {
                        return Err(mcp_response_limit_error());
                    }
                    *encoded = next;
                }
                content.push(block);
            }
            Err(s) => {
                content.push(json!({"type": "text", "text": s.message()}));
                let response = json!({
                    "jsonrpc": "2.0",
                    "id": id,
                    "result": {
                        "content": content,
                        "isError": true,
                        "error": error_payload(&s),
                    },
                });
                return bounded_mcp_stream_response(response, max_response_bytes);
            }
        }
    }
    bounded_mcp_stream_response(
        json!({
            "jsonrpc": "2.0",
            "id": id,
            "result": {"content": content},
        }),
        max_response_bytes,
    )
}

fn bounded_mcp_stream_response(
    response: Value,
    max_response_bytes: Option<usize>,
) -> Result<Value, Status> {
    if max_response_bytes.is_some_and(|limit| {
        serde_json::to_vec(&response)
            .expect("serialize MCP JSON value")
            .len()
            > limit
    }) {
        return Err(mcp_response_limit_error());
    }
    Ok(response)
}

fn mcp_response_limit_error() -> Status {
    Status::resource_exhausted("encoded MCP response exceeds configured byte limit")
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
    let opts = SerializeOptions::new();
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
            "content": [{"type": "text", "text": err.message()}],
            "isError": true,
            "error": error_payload(err),
        },
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    fn test_server() -> Arc<Server> {
        let descriptor = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../python/tests/proto/descriptor.binpb");
        Arc::new(Server::from_descriptor(descriptor.to_str().unwrap()).unwrap())
    }

    #[tokio::test]
    async fn numeric_ids_use_the_portable_safe_integer_range_and_canonical_zero() {
        let server = test_server();
        for raw_id in ["-0", "0", "1.0", "9007199254740991", "-9007199254740991"] {
            let message: Value = serde_json::from_str(&format!(
                r#"{{"jsonrpc":"2.0","id":{raw_id},"method":"ping"}}"#
            ))
            .unwrap();
            let response = mcp_dispatch(&server, &message).await.unwrap();
            let expected: Value = serde_json::from_str(raw_id).unwrap();
            let expected = canonical_jsonrpc_id(expected);
            assert_eq!(response["id"], expected, "{raw_id}");
        }

        for raw_id in ["9007199254740992", "-9007199254740992"] {
            let message: Value = serde_json::from_str(&format!(
                r#"{{"jsonrpc":"2.0","id":{raw_id},"method":"ping"}}"#
            ))
            .unwrap();
            let response = mcp_dispatch(&server, &message).await.unwrap();
            assert!(response["id"].is_null(), "{raw_id}");
            assert_eq!(response["error"]["code"], -32600, "{raw_id}");
        }

        assert_eq!(id_key(serde_json::from_str("-0").unwrap()), "num:0");
        assert_eq!(id_key(json!(0)), "num:0");
    }

    #[tokio::test]
    async fn stdio_rejects_valid_json_with_invalid_request_shapes() {
        let server = test_server();
        let (mut input, session_input) = tokio::io::duplex(4096);
        let (session_output, mut output) = tokio::io::duplex(4096);
        let session = tokio::spawn(serve_mcp_session(
            server,
            session_input,
            session_output,
            CancellationToken::new(),
        ));

        input.write_all(&[0xff, b'\n']).await.unwrap();
        for message in [
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
            json!({"jsonrpc": "2.0", "id": 8, "result": "not-an-object"}),
            json!({"jsonrpc": "2.0", "id": 8, "result": {}, "error": {}}),
            json!({"jsonrpc": "2.0", "result": {}}),
            json!({"jsonrpc": "2.0", "id": null, "error": {"code": -32601, "message": "missing"}}),
            json!({"jsonrpc": "2.0", "error": {"code": 1.5, "message": "bad code"}}),
        ] {
            input
                .write_all(format!("{message}\n").as_bytes())
                .await
                .unwrap();
        }
        for message in [
            json!({"jsonrpc": "2.0", "id": 9, "result": {}}),
            json!({"jsonrpc": "2.0", "id": "response-10", "error": {"code": -32601, "message": "missing"}}),
            json!({"jsonrpc": "2.0", "error": {"code": -32601, "message": "unknown request"}}),
        ] {
            input
                .write_all(format!("{message}\n").as_bytes())
                .await
                .unwrap();
        }
        input
            .write_all(
                format!(
                    "{}\n",
                    json!({"jsonrpc": "2.0", "id": 11, "method": "ping", "params": []})
                )
                .as_bytes(),
            )
            .await
            .unwrap();
        input.shutdown().await.unwrap();
        session.await.unwrap().unwrap();

        let mut bytes = Vec::new();
        output.read_to_end(&mut bytes).await.unwrap();
        let responses = String::from_utf8(bytes)
            .unwrap()
            .lines()
            .map(|line| serde_json::from_str::<Value>(line).unwrap())
            .collect::<Vec<_>>();
        assert_eq!(responses.len(), 18);
        assert_eq!(responses[0]["error"]["code"], -32700);
        assert!(
            responses[1..17]
                .iter()
                .all(|response| response["error"]["code"] == -32600)
        );
        assert!(
            responses[1..17]
                .iter()
                .all(|response| response["id"].is_null())
        );
        assert_eq!(responses[17]["id"], 11);
        assert_eq!(responses[17]["error"]["code"], -32602);
    }

    #[tokio::test]
    async fn stdio_shutdown_aborts_and_joins_in_flight_tool_calls() {
        struct DropSignal(Option<tokio::sync::oneshot::Sender<()>>);

        impl Drop for DropSignal {
            fn drop(&mut self) {
                if let Some(signal) = self.0.take() {
                    let _ = signal.send(());
                }
            }
        }

        let server = test_server();
        let entered = Arc::new(tokio::sync::Notify::new());
        let (dropped_tx, dropped_rx) = tokio::sync::oneshot::channel();
        let dropped_tx = Arc::new(Mutex::new(Some(dropped_tx)));
        server
            .use_shared_unary(Arc::new({
                let entered = entered.clone();
                move |_, _, _| {
                    let entered = entered.clone();
                    let dropped_tx = dropped_tx
                        .lock()
                        .take()
                        .expect("only one tool call is expected");
                    Box::pin(async move {
                        let _signal = DropSignal(Some(dropped_tx));
                        entered.notify_one();
                        futures::future::pending::<Result<crate::server::ErasedResponse, Status>>()
                            .await
                    })
                }
            }))
            .unwrap();
        server
            .connect_http(
                &reqwest::Client::new(),
                reqwest::Url::parse("https://example.test").unwrap(),
            )
            .unwrap();

        let (mut input, session_input) = tokio::io::duplex(4096);
        let (session_output, mut output) = tokio::io::duplex(4096);
        let shutdown = CancellationToken::new();
        let session = tokio::spawn(serve_mcp_session(
            server,
            session_input,
            session_output,
            shutdown.clone(),
        ));
        input
            .write_all(
                format!(
                    "{}\n",
                    json!({
                        "jsonrpc": "2.0",
                        "id": 1,
                        "method": "tools/call",
                        "params": {
                            "name": "greet.v1.GreetService.Greet",
                            "arguments": {"name": "slow"},
                        },
                    })
                )
                .as_bytes(),
            )
            .await
            .unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(2), entered.notified())
            .await
            .unwrap();

        shutdown.cancel();
        tokio::time::timeout(std::time::Duration::from_secs(2), session)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(2), dropped_rx)
            .await
            .unwrap()
            .unwrap();

        let mut bytes = Vec::new();
        output.read_to_end(&mut bytes).await.unwrap();
        assert!(bytes.is_empty());
    }

    #[tokio::test]
    async fn stdio_cancellation_notification_aborts_the_matching_call_without_a_response() {
        struct DropSignal(Option<tokio::sync::oneshot::Sender<()>>);

        impl Drop for DropSignal {
            fn drop(&mut self) {
                if let Some(signal) = self.0.take() {
                    let _ = signal.send(());
                }
            }
        }

        let server = test_server();
        let entered = Arc::new(tokio::sync::Notify::new());
        let (dropped_tx, dropped_rx) = tokio::sync::oneshot::channel();
        let dropped_tx = Arc::new(Mutex::new(Some(dropped_tx)));
        server
            .use_shared_unary(Arc::new({
                let entered = entered.clone();
                move |_, _, _| {
                    let entered = entered.clone();
                    let dropped_tx = dropped_tx
                        .lock()
                        .take()
                        .expect("only one tool call is expected");
                    Box::pin(async move {
                        let _signal = DropSignal(Some(dropped_tx));
                        entered.notify_one();
                        futures::future::pending::<Result<crate::server::ErasedResponse, Status>>()
                            .await
                    })
                }
            }))
            .unwrap();
        server
            .connect_http(
                &reqwest::Client::new(),
                reqwest::Url::parse("https://example.test").unwrap(),
            )
            .unwrap();

        let (mut input, session_input) = tokio::io::duplex(4096);
        let (session_output, mut output) = tokio::io::duplex(4096);
        let session = tokio::spawn(serve_mcp_session(
            server,
            session_input,
            session_output,
            CancellationToken::new(),
        ));
        input
            .write_all(
                format!(
                    "{}\n",
                    json!({
                        "jsonrpc": "2.0",
                        "id": "cancel-me",
                        "method": "tools/call",
                        "params": {
                            "name": "greet.v1.GreetService.Greet",
                            "arguments": {"name": "slow"},
                        },
                    })
                )
                .as_bytes(),
            )
            .await
            .unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(2), entered.notified())
            .await
            .unwrap();
        input
            .write_all(
                format!(
                    "{}\n",
                    json!({
                        "jsonrpc": "2.0",
                        "method": "notifications/cancelled",
                        "params": {"requestId": "cancel-me"},
                    })
                )
                .as_bytes(),
            )
            .await
            .unwrap();
        input.shutdown().await.unwrap();

        tokio::time::timeout(std::time::Duration::from_secs(2), session)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(2), dropped_rx)
            .await
            .unwrap()
            .unwrap();

        let mut bytes = Vec::new();
        output.read_to_end(&mut bytes).await.unwrap();
        assert!(bytes.is_empty());
    }
}
