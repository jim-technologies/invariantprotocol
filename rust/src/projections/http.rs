//! HTTP / Connect projection.
//!
//! Wire format: Connect protocol — `POST /{package.Service}/{Method}` with
//! `application/json` or `application/proto` for unary. Server-streaming
//! envelopes (`application/connect+json` / `application/connect+proto`) are
//! sketched in the route table but the streaming dispatch path is left to
//! the next milestone (parity-with-Go for unary first).
//!
//! Same hand-rolled approach as `go/http.go` and
//! `python/.../projections/http.py`: we don't depend on a Connect client
//! library because (a) we want descriptor-driven dispatch, not codegen, and
//! (b) the protocol itself is small enough that pulling a library costs
//! more in dep weight than re-implementing.

use crate::errors::{Code, Status};
use crate::server::{Server, Tool};
use axum::{
    Router,
    body::Bytes,
    extract::{Request, State},
    http::{HeaderMap, HeaderValue, Method, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use http_body_util::BodyExt;
use prost::Message;
use prost_reflect::DynamicMessage;
use serde_json::json;
use std::sync::Arc;
use std::time::Duration;

pub const HTTP_MAX_UNARY_REQUEST: usize = 16 * 1024 * 1024;
pub const CONNECT_STREAM_MAX_REQUEST: usize = 16 * 1024 * 1024;
pub const CONTENT_TYPE_JSON: &str = "application/json";
pub const CONTENT_TYPE_PROTO: &str = "application/proto";
pub const CONNECT_STREAM_JSON: &str = "application/connect+json";
pub const CONNECT_STREAM_PROTO: &str = "application/connect+proto";
pub const CONNECT_END_STREAM_FLAG: u8 = 0x02;
pub const CONNECT_TIMEOUT_HEADER: &str = "connect-timeout-ms";

/// Build the axum `Router` mounting all Connect tool endpoints + catalog +
/// health + MCP HTTP transport. Mount under any prefix via `Router::nest`.
pub fn http_router(server: Arc<Server>) -> Router {
    let mut router = Router::new()
        .route("/", get(catalog_handler))
        .route("/__invariant/tools", get(catalog_handler))
        .route("/__invariant/descriptor.binpb", get(descriptor_handler))
        .route("/healthz", get(health_handler))
        .route("/readyz", get(health_handler))
        .route("/mcp", post(mcp_http_handler));

    for tool in server.tools_snapshot() {
        let path = format!("/{}/{}", tool.service_full_name, tool.method_name);
        router = router.route(&path, post(tool_handler));
    }

    router.fallback(not_found).with_state(server)
}

/// Bind a `TcpListener` and serve forever. Honours ctx via tokio cancellation —
/// callers wanting graceful shutdown should spawn this on a task and abort it.
pub async fn serve_http(server: Arc<Server>, port: u16) -> std::io::Result<()> {
    let app = http_router(server);
    let listener = tokio::net::TcpListener::bind(("0.0.0.0", port)).await?;
    axum::serve(listener, app).await
}

// ---------- handlers ----------

async fn catalog_handler(State(server): State<Arc<Server>>) -> impl IntoResponse {
    let catalog = server.tool_catalog();
    json_response(StatusCode::OK, &json!({"tools": catalog}))
}

async fn descriptor_handler(State(server): State<Arc<Server>>) -> impl IntoResponse {
    let bytes = server.parsed().raw_fds.clone();
    (
        StatusCode::OK,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_PROTO),
        )],
        bytes,
    )
        .into_response()
}

async fn health_handler() -> impl IntoResponse {
    json_response(StatusCode::OK, &json!({"status": "ok"}))
}

async fn not_found() -> impl IntoResponse {
    error_response(&Status::not_found("not found"))
}

async fn mcp_http_handler(State(server): State<Arc<Server>>, req: Request) -> Response {
    let body = match read_limited_body(req, server.max_unary_request_bytes()).await {
        Ok(b) => b,
        Err(s) => return error_response(&s),
    };
    let msg: serde_json::Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(e) => {
            let resp = serde_json::json!({
                "jsonrpc": "2.0",
                "id": null,
                "error": {"code": -32700, "message": format!("Parse error: {e}")},
            });
            return json_response(StatusCode::OK, &resp);
        }
    };
    match crate::projections::mcp::mcp_dispatch(&server, &msg).await {
        Some(resp) => json_response(StatusCode::OK, &resp),
        None => (StatusCode::NO_CONTENT, "").into_response(),
    }
}

async fn tool_handler(State(server): State<Arc<Server>>, req: Request) -> Response {
    if req.method() != Method::POST {
        return (StatusCode::METHOD_NOT_ALLOWED, "").into_response();
    }
    let path = req.uri().path().to_string();
    let tool = match find_tool_by_path(&server, &path) {
        Some(t) => t,
        None => return error_response(&Status::not_found(format!("unknown tool path {path:?}"))),
    };

    if tool.server_streaming {
        return stream_tool_handler(server, tool, req).await;
    }

    let headers = req.headers().clone();
    let timeout = parse_connect_timeout(&headers);

    let body_limit = server.max_unary_request_bytes();
    let body = match read_limited_body(req, body_limit).await {
        Ok(b) => b,
        Err(s) => return error_response(&s),
    };

    let content_type = content_type(&headers);
    let want_proto = is_proto(content_type.as_deref());
    let wants_proto_response = is_proto(headers.get(header::ACCEPT).and_then(|v| v.to_str().ok()));

    let dyn_req = match decode_request(&tool, want_proto, &body) {
        Ok(d) => d,
        Err(s) => return error_response(&s),
    };

    let invoke = invoke_tool(server.clone(), tool.clone(), dyn_req);
    let resp = match timeout {
        Some(d) => match tokio::time::timeout(d, invoke).await {
            Ok(r) => r,
            Err(_) => Err(Status::deadline_exceeded(format!(
                "deadline exceeded after {}ms",
                d.as_millis()
            ))),
        },
        None => invoke.await,
    };

    match resp {
        Ok(dyn_resp) => encode_response(&dyn_resp, wants_proto_response),
        Err(s) => error_response(&s),
    }
}

/// Connect server-streaming wire path.
///
/// Request body is a single envelope wrapping the request (JSON or binary
/// proto). Response body is zero or more message envelopes, then one
/// end-stream envelope (flags=0x02) carrying `{}` on success or
/// `{"error": {...}}` on failure. End-stream payload is always JSON
/// regardless of message content-type — per Connect spec.
async fn stream_tool_handler(
    server: Arc<Server>,
    tool: Arc<crate::server::Tool>,
    req: Request,
) -> Response {
    let headers = req.headers().clone();
    let ct = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok());
    let binary = matches_ct(ct, CONNECT_STREAM_PROTO);
    if !binary && !matches_ct(ct, CONNECT_STREAM_JSON) {
        return error_response(&Status::invalid_argument(format!(
            "streaming tools require Content-Type: {CONNECT_STREAM_JSON} or {CONNECT_STREAM_PROTO}"
        )));
    }

    let stream_cap = server.max_stream_request_bytes();
    let body = match read_limited_body(req, stream_cap).await {
        Ok(b) => b,
        Err(s) => return error_response(&s),
    };

    // Single envelope on the request side.
    let req_bytes = match unpack_envelope(&body) {
        Ok((_flags, data)) => data,
        Err(s) => return error_response(&s),
    };

    let dyn_req = match decode_request(&tool, binary, req_bytes) {
        Ok(d) => d,
        Err(s) => return error_response(&s),
    };

    let resp_ct = if binary {
        CONNECT_STREAM_PROTO
    } else {
        CONNECT_STREAM_JSON
    };
    let timeout = parse_connect_timeout(&headers);
    let stream = server.invoke_stream(&tool.name, dyn_req);
    let body_stream = build_connect_stream(stream, binary, timeout);

    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, resp_ct)
        .body(axum::body::Body::from_stream(body_stream))
        .unwrap_or_else(|_| (StatusCode::INTERNAL_SERVER_ERROR, "").into_response())
}

fn build_connect_stream(
    mut messages: futures::stream::BoxStream<'static, Result<DynamicMessage, Status>>,
    binary: bool,
    timeout: Option<Duration>,
) -> futures::stream::BoxStream<'static, Result<Bytes, std::io::Error>> {
    use futures::StreamExt;
    Box::pin(async_stream::stream! {
        let deadline = timeout.map(|d| tokio::time::Instant::now() + d);
        let mut end_payload = Bytes::from_static(b"{}");

        loop {
            let item = if let Some(d) = deadline {
                match tokio::time::timeout_at(d, messages.next()).await {
                    Ok(it) => it,
                    Err(_) => {
                        let err = Status::deadline_exceeded(format!(
                            "deadline exceeded after {}ms",
                            timeout.unwrap_or_default().as_millis()
                        ));
                        end_payload = end_envelope_for_error(&err);
                        break;
                    }
                }
            } else {
                messages.next().await
            };
            match item {
                None => break,
                Some(Ok(msg)) => {
                    let payload = if binary {
                        msg.encode_to_vec()
                    } else {
                        let opts = prost_reflect::SerializeOptions::new().use_proto_field_name(true);
                        let mut buf = Vec::with_capacity(128);
                        let mut ser = serde_json::Serializer::new(&mut buf);
                        if msg.serialize_with_options(&mut ser, &opts).is_err() {
                            let err = Status::internal("marshal stream chunk");
                            end_payload = end_envelope_for_error(&err);
                            break;
                        }
                        buf
                    };
                    yield Ok(Bytes::from(pack_envelope(0, &payload)));
                }
                Some(Err(s)) => {
                    end_payload = end_envelope_for_error(&s);
                    break;
                }
            }
        }

        yield Ok(Bytes::from(pack_envelope(CONNECT_END_STREAM_FLAG, &end_payload)));
    })
}

fn end_envelope_for_error(err: &Status) -> Bytes {
    let payload = serde_json::json!({"error": err.to_payload()});
    Bytes::from(serde_json::to_vec(&payload).unwrap_or_default())
}

fn pack_envelope(flags: u8, payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(5 + payload.len());
    out.push(flags);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

fn unpack_envelope(data: &[u8]) -> Result<(u8, &[u8]), Status> {
    if data.len() < 5 {
        return Err(Status::invalid_argument(
            "stream request body shorter than envelope header",
        ));
    }
    let flags = data[0];
    let size = u32::from_be_bytes([data[1], data[2], data[3], data[4]]) as usize;
    if size > CONNECT_STREAM_MAX_REQUEST {
        return Err(Status::invalid_argument(format!(
            "envelope size {size} exceeds {CONNECT_STREAM_MAX_REQUEST}"
        )));
    }
    if data.len() < 5 + size {
        return Err(Status::invalid_argument("stream request body truncated"));
    }
    Ok((flags, &data[5..5 + size]))
}

fn matches_ct(value: Option<&str>, want: &str) -> bool {
    let Some(v) = value else { return false };
    v.split(';').next().unwrap_or("").trim() == want
}

async fn invoke_tool(
    server: Arc<Server>,
    tool: Arc<Tool>,
    request: DynamicMessage,
) -> Result<DynamicMessage, Status> {
    server.chained_invoke(tool, request).await
}

// ---------- request / response helpers ----------

fn find_tool_by_path(server: &Server, path: &str) -> Option<Arc<Tool>> {
    // path format: /{package.Service}/{Method}
    let stripped = path.strip_prefix('/')?;
    let (service, method) = stripped.rsplit_once('/')?;
    server
        .tools_snapshot()
        .into_iter()
        .find(|t| t.service_full_name == service && t.method_name == method)
}

fn parse_connect_timeout(headers: &HeaderMap) -> Option<Duration> {
    let raw = headers
        .get(CONNECT_TIMEOUT_HEADER)
        .or_else(|| headers.get("Connect-Timeout-Ms"))?
        .to_str()
        .ok()?;
    let ms: u64 = raw.trim().parse().ok()?;
    if ms == 0 {
        return None;
    }
    Some(Duration::from_millis(ms))
}

fn content_type(headers: &HeaderMap) -> Option<String> {
    headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string())
}

fn is_proto(ct: Option<&str>) -> bool {
    let Some(ct) = ct else { return false };
    ct.split(';').next().unwrap_or("").trim() == CONTENT_TYPE_PROTO
}

async fn read_limited_body(req: Request, max: usize) -> Result<Bytes, Status> {
    // Hyper's `Limited` body would stop midstream but we get a cleaner error
    // shape by collecting and checking — request bodies bounded by `max` never
    // exceed the 16 MiB ceiling and tests assert the `resource_exhausted` code.
    let collected = req
        .into_body()
        .collect()
        .await
        .map_err(|e| Status::invalid_argument(format!("read body: {e}")))?;
    let bytes = collected.to_bytes();
    if bytes.len() > max {
        return Err(Status::resource_exhausted(format!(
            "request body exceeds {max} byte limit"
        )));
    }
    Ok(bytes)
}

fn decode_request(tool: &Tool, want_proto: bool, body: &[u8]) -> Result<DynamicMessage, Status> {
    if want_proto {
        return DynamicMessage::decode(tool.input_desc.clone(), body)
            .map_err(|e| Status::invalid_argument(format!("decode binary proto: {e}")));
    }
    if body.is_empty() || body.iter().all(|b| b.is_ascii_whitespace()) {
        return Ok(DynamicMessage::new(tool.input_desc.clone()));
    }
    let mut deserializer = serde_json::Deserializer::from_slice(body);
    let opts = prost_reflect::DeserializeOptions::new();
    DynamicMessage::deserialize_with_options(tool.input_desc.clone(), &mut deserializer, &opts)
        .map_err(|e| invalid_argument_from_json_error(&e.to_string()))
}

fn encode_response(msg: &DynamicMessage, want_proto: bool) -> Response {
    if want_proto {
        let bytes = msg.encode_to_vec();
        return (
            StatusCode::OK,
            [(
                header::CONTENT_TYPE,
                HeaderValue::from_static(CONTENT_TYPE_PROTO),
            )],
            bytes,
        )
            .into_response();
    }
    let opts = prost_reflect::SerializeOptions::new().use_proto_field_name(true);
    let mut buf = Vec::with_capacity(128);
    let mut ser = serde_json::Serializer::new(&mut buf);
    if let Err(e) = msg.serialize_with_options(&mut ser, &opts) {
        return error_response(&Status::internal(format!("marshal response: {e}")));
    }
    (
        StatusCode::OK,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_JSON),
        )],
        buf,
    )
        .into_response()
}

fn json_response(status: StatusCode, value: &serde_json::Value) -> Response {
    let body = serde_json::to_vec(value).unwrap_or_default();
    (
        status,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_JSON),
        )],
        body,
    )
        .into_response()
}

fn error_response(status: &Status) -> Response {
    let body = serde_json::to_vec(&status.to_payload()).unwrap_or_default();
    let http_status = StatusCode::from_u16(status.code.http_status())
        .unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    (
        http_status,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_JSON),
        )],
        body,
    )
        .into_response()
}

/// Extract `unknown field "..."` from protobuf JSON parse errors and surface
/// it as a structured `BadRequest` detail. Mirrors Go's
/// `invalidArgumentFromJSONError`.
fn invalid_argument_from_json_error(msg: &str) -> Status {
    let mut field = None;
    for needle in ["unknown field \"", "no field named \""] {
        if let Some(start) = msg.find(needle) {
            let after = &msg[start + needle.len()..];
            if let Some(end) = after.find('"') {
                field = Some(after[..end].to_string());
                break;
            }
        }
    }
    let mut s = Status::invalid_argument(format!("proto: {msg}"));
    if let Some(f) = field {
        s = s.with_detail(json!({
            "@type": "type.googleapis.com/google.rpc.BadRequest",
            "fieldViolations": [{"field": f, "description": format!("proto: {msg}")}],
        }));
    }
    s
}

// ---------- Code path constants kept for symmetry with Go ----------
const _: Code = Code::Ok;
