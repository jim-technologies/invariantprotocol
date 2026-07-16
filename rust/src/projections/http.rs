//! HTTP / Connect projection.
//!
//! Wire format: Connect protocol — `POST /{package.Service}/{Method}` with
//! `application/json` or `application/proto` for unary and Connect envelopes
//! (`application/connect+json` / `application/connect+proto`) for
//! server-streaming methods.
//!
//! This projection remains intentionally narrow. The official pre-1.0 Rust
//! Connect implementation currently generates Buffa messages and service
//! traits; adopting it would introduce a second application type/handler model
//! beside canonical Prost/Tonic services.

use crate::errors::{error_payload, http_status};
use crate::server::{ProjectionContext, ProjectionGuard, Server, Tool};
use axum::{
    Router,
    body::Bytes,
    extract::{Request, State},
    http::{Extensions, HeaderMap, HeaderName, HeaderValue, Method, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{any, get, post},
};
use base64::Engine;
use http_body_util::{BodyExt, LengthLimitError, Limited};
use prost::Message;
use prost_reflect::DynamicMessage;
use serde_json::json;
use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;
use tonic::metadata::{KeyAndValueRef, MetadataMap};
use tonic::{Code, Request as GrpcRequest, Response as GrpcResponse, Status};
use tonic_types::{ErrorDetails, StatusExt};

pub const CONTENT_TYPE_JSON: &str = "application/json";
pub const CONTENT_TYPE_PROTO: &str = "application/proto";
pub const CONNECT_STREAM_JSON: &str = "application/connect+json";
pub const CONNECT_STREAM_PROTO: &str = "application/connect+proto";
pub const CONNECT_END_STREAM_FLAG: u8 = 0x02;
pub const CONNECT_TIMEOUT_HEADER: &str = "connect-timeout-ms";
const CONNECT_CONTROL_MAX: usize = 1024 * 1024;
const RESOURCE_EXHAUSTED_END_STREAM: &[u8] = br#"{"error":{"code":"resource_exhausted"}}"#;

/// Build the axum `Router` mounting all Connect tool endpoints + catalog +
/// health + MCP HTTP transport. Mount under any prefix via `Router::nest`.
pub fn http_router(server: Arc<Server>) -> Router {
    server.freeze();
    let mut router = Router::new()
        .route("/", get(catalog_handler))
        .route("/__invariant/tools", get(catalog_handler))
        .route("/__invariant/descriptor.binpb", get(descriptor_handler))
        .route("/healthz", get(health_handler))
        .route("/readyz", get(health_handler))
        .route("/mcp", any(mcp_http_handler));

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
    if req.headers().contains_key(header::ORIGIN) {
        return (StatusCode::FORBIDDEN, "").into_response();
    }
    if req.method() != Method::POST {
        return (StatusCode::METHOD_NOT_ALLOWED, "").into_response();
    }
    if !accepts_media_type(req.headers(), CONTENT_TYPE_JSON)
        || !accepts_media_type(req.headers(), "text/event-stream")
    {
        return (StatusCode::NOT_ACCEPTABLE, "").into_response();
    }
    if !matches_ct(
        req.headers()
            .get(header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok()),
        CONTENT_TYPE_JSON,
    ) {
        return (StatusCode::UNSUPPORTED_MEDIA_TYPE, "").into_response();
    }
    let max_response_bytes = server.max_unary_response_bytes();
    let headers = req.headers().clone();
    let protocol_version = headers
        .get("mcp-protocol-version")
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .map(str::to_string);
    let timeout = match parse_connect_timeout(&headers) {
        Ok(timeout) => timeout,
        Err(status) => return error_response_with_limit(&status, max_response_bytes),
    };
    let projection = ProjectionContext::new(timeout);
    let _guard = ProjectionGuard::new(projection.clone());
    let body = match until_projection_deadline(
        &projection,
        read_limited_body(req, server.max_unary_request_bytes()),
    )
    .await
    {
        Ok(b) => b,
        Err(s) => return error_response_with_limit(&s, max_response_bytes),
    };
    let msg: serde_json::Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(e) => {
            let resp = serde_json::json!({
                "jsonrpc": "2.0",
                "id": null,
                "error": {"code": -32700, "message": format!("Parse error: {e}")},
            });
            if let Some(status) = projection_deadline_error(&projection) {
                return error_response_with_limit(&status, max_response_bytes);
            }
            return mcp_json_response(StatusCode::OK, &resp, max_response_bytes);
        }
    };
    if let Some(response) = crate::projections::mcp::invalid_request_response(&msg) {
        if let Some(status) = projection_deadline_error(&projection) {
            return error_response_with_limit(&status, max_response_bytes);
        }
        return mcp_json_response(StatusCode::OK, &response, max_response_bytes);
    }
    let response_message = crate::projections::mcp::is_client_response(&msg);
    let initialize = msg.get("method").and_then(|value| value.as_str()) == Some("initialize");
    let version_valid = matches!(
        (initialize, protocol_version.as_deref()),
        (
            true,
            None | Some(crate::projections::mcp::MCP_PROTOCOL_VERSION)
        ) | (false, Some(crate::projections::mcp::MCP_PROTOCOL_VERSION))
    );
    if !version_valid {
        if let Some(status) = projection_deadline_error(&projection) {
            return error_response_with_limit(&status, max_response_bytes);
        }
        return (StatusCode::BAD_REQUEST, "").into_response();
    }
    if response_message {
        if let Some(status) = projection_deadline_error(&projection) {
            return error_response_with_limit(&status, max_response_bytes);
        }
        return mcp_empty_response(StatusCode::ACCEPTED);
    }
    let metadata = server.incoming_http_metadata(&headers);
    let dispatch = crate::projections::mcp::mcp_dispatch_with_context(
        &server,
        &msg,
        metadata,
        Some(projection.clone()),
    );
    match until_projection_deadline(&projection, async { Ok(dispatch.await) }).await {
        Ok(Some(resp)) => mcp_json_response(StatusCode::OK, &resp, max_response_bytes),
        Ok(None) => mcp_empty_response(StatusCode::ACCEPTED),
        Err(status) => error_response_with_limit(&status, max_response_bytes),
    }
}

fn accepts_media_type(headers: &HeaderMap, expected: &str) -> bool {
    headers.get_all(header::ACCEPT).iter().any(|value| {
        value.to_str().is_ok_and(|value| {
            value.split(',').any(|candidate| {
                let mut parts = candidate.split(';');
                if !parts
                    .next()
                    .unwrap_or_default()
                    .trim()
                    .eq_ignore_ascii_case(expected)
                {
                    return false;
                }
                parts.all(|parameter| {
                    let Some((name, value)) = parameter.trim().split_once('=') else {
                        return true;
                    };
                    !name.trim().eq_ignore_ascii_case("q")
                        || value
                            .trim()
                            .parse::<f32>()
                            .is_ok_and(|quality| quality > 0.0)
                })
            })
        })
    })
}

fn mcp_json_response(
    status: StatusCode,
    value: &serde_json::Value,
    max_response_bytes: usize,
) -> Response {
    let body = serde_json::to_vec(value).unwrap_or_default();
    if max_response_bytes > 0 && body.len() > max_response_bytes {
        return error_response_with_limit(
            &Status::resource_exhausted("encoded MCP response exceeds configured byte limit"),
            max_response_bytes,
        );
    }
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, CONTENT_TYPE_JSON)
        .header(
            "mcp-protocol-version",
            crate::projections::mcp::MCP_PROTOCOL_VERSION,
        )
        .body(axum::body::Body::from(body))
        .unwrap_or_else(|_| (StatusCode::INTERNAL_SERVER_ERROR, "").into_response())
}

fn mcp_empty_response(status: StatusCode) -> Response {
    Response::builder()
        .status(status)
        .header(
            "mcp-protocol-version",
            crate::projections::mcp::MCP_PROTOCOL_VERSION,
        )
        .body(axum::body::Body::empty())
        .unwrap_or_else(|_| (StatusCode::INTERNAL_SERVER_ERROR, "").into_response())
}

async fn tool_handler(State(server): State<Arc<Server>>, req: Request) -> Response {
    if req.method() != Method::POST {
        return (StatusCode::METHOD_NOT_ALLOWED, "").into_response();
    }
    let path = req.uri().path().to_string();
    let tool = match find_tool_by_path(&server, &path) {
        Some(t) => t,
        None => {
            return error_response_with_limit(
                &Status::not_found(format!("unknown tool path {path:?}")),
                server.max_unary_response_bytes(),
            );
        }
    };

    if tool.server_streaming {
        return stream_tool_handler(server, tool, req).await;
    }

    let headers = req.headers().clone();
    let limits = server.http_limits(&tool);
    let content_type = content_type(&headers);
    let json_request = matches_ct(content_type.as_deref(), CONTENT_TYPE_JSON);
    let proto_request = is_proto(content_type.as_deref());
    if !json_request && !proto_request {
        return unsupported_media_type_response(
            &format!(
                "unary tools require Content-Type: {CONTENT_TYPE_JSON} or {CONTENT_TYPE_PROTO}"
            ),
            limits.unary_response,
        );
    }
    let content_encoding = headers
        .get(header::CONTENT_ENCODING)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();
    if !content_encoding.is_empty() && content_encoding != "identity" {
        return error_response_with_limit(
            &Status::unimplemented(format!(
                "Content-Encoding {content_encoding:?} is not supported"
            )),
            limits.unary_response,
        );
    }
    let timeout = match parse_connect_timeout(&headers) {
        Ok(timeout) => timeout,
        Err(status) => return error_response_with_limit(&status, limits.unary_response),
    };
    let projection = ProjectionContext::new(timeout);
    let _guard = ProjectionGuard::new(projection.clone());
    let mut req = req;
    let extensions = std::mem::take(req.extensions_mut());

    let body =
        match until_projection_deadline(&projection, read_limited_body(req, limits.unary_request))
            .await
        {
            Ok(b) => b,
            Err(s) => return error_response_with_limit(&s, limits.unary_response),
        };

    let wants_proto_response = is_proto(headers.get(header::ACCEPT).and_then(|v| v.to_str().ok()));

    let dyn_req = match decode_request(&tool, proto_request, &body) {
        Ok(d) => d,
        Err(s) => return error_response_with_limit(&s, limits.unary_response),
    };

    let request = projection_request(&server, &headers, extensions, projection.clone(), dyn_req);
    let resp = until_projection_deadline(&projection, server.invoke(&tool.name, request)).await;

    match resp {
        Ok(response) => encode_response(response, wants_proto_response, limits.unary_response),
        Err(s) => error_response_with_limit(&s, limits.unary_response),
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
    let limits = server.http_limits(&tool);
    let ct = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok());
    let binary = matches_ct(ct, CONNECT_STREAM_PROTO);
    if !binary && !matches_ct(ct, CONNECT_STREAM_JSON) {
        return unsupported_media_type_response(
            &format!(
                "streaming tools require Content-Type: {CONNECT_STREAM_JSON} or {CONNECT_STREAM_PROTO}"
            ),
            limits.stream_response,
        );
    }
    let resp_ct = if binary {
        CONNECT_STREAM_PROTO
    } else {
        CONNECT_STREAM_JSON
    };
    let timeout = match parse_connect_timeout(&headers) {
        Ok(timeout) => timeout,
        Err(status) => {
            return connect_stream_error_response(resp_ct, &status);
        }
    };
    let projection = ProjectionContext::new(timeout);
    let guard = ProjectionGuard::new(projection.clone());
    let mut req = req;
    let extensions = std::mem::take(req.extensions_mut());

    let body = match until_projection_deadline(
        &projection,
        read_limited_body(req, limits.stream_request.saturating_add(5)),
    )
    .await
    {
        Ok(b) => b,
        Err(s) => {
            return connect_stream_error_response(resp_ct, &s);
        }
    };

    // Single envelope on the request side.
    let req_bytes = match unpack_envelope(&body, limits.stream_request) {
        Ok((_flags, data)) => data,
        Err(s) => {
            return connect_stream_error_response(resp_ct, &s);
        }
    };

    let dyn_req = match decode_request(&tool, binary, req_bytes) {
        Ok(d) => d,
        Err(s) => {
            return connect_stream_error_response(resp_ct, &s);
        }
    };

    let request = projection_request(&server, &headers, extensions, projection.clone(), dyn_req);
    let response =
        match until_projection_deadline(&projection, server.invoke_stream(&tool.name, request))
            .await
        {
            Ok(response) => response,
            Err(status) => {
                return connect_stream_error_response(resp_ct, &status);
            }
        };
    let (metadata, stream, _) = response.into_parts();
    let body_stream =
        build_connect_stream(stream, binary, projection, limits.stream_response, guard);

    let mut response = Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, resp_ct)
        .body(axum::body::Body::from_stream(body_stream))
        .unwrap_or_else(|_| (StatusCode::INTERNAL_SERVER_ERROR, "").into_response());
    append_response_metadata(response.headers_mut(), &metadata, false);
    response
}

fn build_connect_stream(
    mut messages: futures::stream::BoxStream<'static, Result<DynamicMessage, Status>>,
    binary: bool,
    projection: ProjectionContext,
    max_response_bytes: usize,
    _guard: ProjectionGuard,
) -> futures::stream::BoxStream<'static, Result<Bytes, std::io::Error>> {
    use futures::StreamExt;
    Box::pin(async_stream::stream! {
        let mut end_payload = Bytes::from_static(b"{}");

        loop {
            let item = match until_projection_deadline(
                &projection,
                async { Ok(messages.next().await) },
            )
            .await
            {
                Ok(item) => item,
                Err(err) => {
                    end_payload = end_envelope_for_error(&err);
                    break;
                }
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
                    if payload.len() > max_response_bytes {
                        let err = Status::resource_exhausted(format!(
                            "stream response message exceeds {max_response_bytes} byte limit"
                        ));
                        end_payload = end_envelope_for_error(&err);
                        break;
                    }
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
    let metadata = connect_end_stream_metadata(err.metadata());
    let payload = if metadata.is_empty() {
        serde_json::json!({"error": error_payload(err)})
    } else {
        serde_json::json!({"error": error_payload(err), "metadata": metadata})
    };
    let encoded = serde_json::to_vec(&payload).unwrap_or_default();
    if encoded.len() <= CONNECT_CONTROL_MAX {
        return Bytes::from(encoded);
    }
    Bytes::from_static(RESOURCE_EXHAUSTED_END_STREAM)
}

fn connect_stream_error_response(content_type: &'static str, status: &Status) -> Response {
    let payload = end_envelope_for_error(status);
    (
        StatusCode::OK,
        [(header::CONTENT_TYPE, HeaderValue::from_static(content_type))],
        pack_envelope(CONNECT_END_STREAM_FLAG, &payload),
    )
        .into_response()
}

fn pack_envelope(flags: u8, payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(5 + payload.len());
    out.push(flags);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

fn unpack_envelope(data: &[u8], max_message_bytes: usize) -> Result<(u8, &[u8]), Status> {
    if data.len() < 5 {
        return Err(Status::invalid_argument(
            "stream request body shorter than envelope header",
        ));
    }
    let flags = data[0];
    if flags & !0x03 != 0 {
        return Err(Status::invalid_argument(format!(
            "request envelope has unsupported reserved flags 0x{:02x}",
            flags & !0x03
        )));
    }
    if flags & 0x01 != 0 {
        return Err(Status::unimplemented(
            "compressed request envelopes are not supported",
        ));
    }
    if flags & CONNECT_END_STREAM_FLAG != 0 {
        return Err(Status::invalid_argument(
            "request envelope must not use the end-stream flag",
        ));
    }
    let size = u32::from_be_bytes([data[1], data[2], data[3], data[4]]) as usize;
    if size > max_message_bytes {
        return Err(Status::resource_exhausted(format!(
            "stream request message exceeds {max_message_bytes} byte limit"
        )));
    }
    if data.len() < 5 + size {
        return Err(Status::invalid_argument("stream request body truncated"));
    }
    if data.len() != 5 + size {
        return Err(Status::invalid_argument(
            "stream request body must contain exactly one envelope",
        ));
    }
    Ok((flags, &data[5..5 + size]))
}

fn matches_ct(value: Option<&str>, want: &str) -> bool {
    let Some(v) = value else { return false };
    v.split(';')
        .next()
        .unwrap_or("")
        .trim()
        .eq_ignore_ascii_case(want)
}

fn projection_request(
    server: &Server,
    headers: &HeaderMap,
    extensions: Extensions,
    projection: ProjectionContext,
    message: DynamicMessage,
) -> GrpcRequest<DynamicMessage> {
    let mut request = GrpcRequest::new(message);
    *request.metadata_mut() = server.incoming_http_metadata(headers);
    *request.extensions_mut() = extensions;
    if let Some(remaining) = projection.remaining() {
        request.set_timeout(remaining);
    }
    request.extensions_mut().insert(projection);
    request
}

async fn until_projection_deadline<T>(
    projection: &ProjectionContext,
    future: impl std::future::Future<Output = Result<T, Status>>,
) -> Result<T, Status> {
    let Some(deadline) = projection.deadline() else {
        return future.await;
    };
    if let Some(status) = projection_deadline_error(projection) {
        return Err(status);
    }
    match tokio::time::timeout_at(deadline, future).await {
        Ok(result) => projection_deadline_error(projection).map_or(result, Err),
        Err(_) => {
            projection.cancel();
            Err(Status::deadline_exceeded("projection deadline exceeded"))
        }
    }
}

fn projection_deadline_error(projection: &ProjectionContext) -> Option<Status> {
    projection
        .deadline()
        .is_some_and(|deadline| tokio::time::Instant::now() >= deadline)
        .then(|| {
            projection.cancel();
            Status::deadline_exceeded("projection deadline exceeded")
        })
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

fn parse_connect_timeout(headers: &HeaderMap) -> Result<Option<Duration>, Status> {
    let Some(value) = headers.get(CONNECT_TIMEOUT_HEADER) else {
        return Ok(None);
    };
    let raw = value.to_str().map_err(|_| {
        Status::invalid_argument("Connect-Timeout-Ms must be a positive ASCII integer")
    })?;
    if raw.is_empty() || raw.len() > 10 || raw.as_bytes().iter().any(|byte| !byte.is_ascii_digit())
    {
        return Err(Status::invalid_argument(
            "Connect-Timeout-Ms must contain 1 to 10 ASCII digits",
        ));
    }
    let ms: u64 = raw
        .parse()
        .map_err(|_| Status::invalid_argument("Connect-Timeout-Ms is out of range"))?;
    if ms == 0 {
        return Err(Status::invalid_argument(
            "Connect-Timeout-Ms must be greater than zero",
        ));
    }
    Ok(Some(Duration::from_millis(ms)))
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
    let collected = Limited::new(req.into_body(), max)
        .collect()
        .await
        .map_err(|error| {
            if error.downcast_ref::<LengthLimitError>().is_some() {
                Status::resource_exhausted(format!("request body exceeds {max} byte limit"))
            } else {
                Status::invalid_argument(format!("read body: {error}"))
            }
        })?;
    Ok(collected.to_bytes())
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

fn encode_response(
    response: GrpcResponse<DynamicMessage>,
    want_proto: bool,
    max_response_bytes: usize,
) -> Response {
    let (metadata, msg, _) = response.into_parts();
    if want_proto {
        let bytes = msg.encode_to_vec();
        if bytes.len() > max_response_bytes {
            return error_response_with_limit(
                &Status::resource_exhausted(format!(
                    "unary response exceeds {max_response_bytes} byte limit"
                )),
                max_response_bytes,
            );
        }
        let mut response = (
            StatusCode::OK,
            [(
                header::CONTENT_TYPE,
                HeaderValue::from_static(CONTENT_TYPE_PROTO),
            )],
            bytes,
        )
            .into_response();
        append_response_metadata(response.headers_mut(), &metadata, false);
        return response;
    }
    let opts = prost_reflect::SerializeOptions::new().use_proto_field_name(true);
    let mut buf = Vec::with_capacity(128);
    let mut ser = serde_json::Serializer::new(&mut buf);
    if let Err(e) = msg.serialize_with_options(&mut ser, &opts) {
        return error_response_with_limit(
            &Status::internal(format!("marshal response: {e}")),
            max_response_bytes,
        );
    }
    if buf.len() > max_response_bytes {
        return error_response_with_limit(
            &Status::resource_exhausted(format!(
                "unary response exceeds {max_response_bytes} byte limit"
            )),
            max_response_bytes,
        );
    }
    let mut response = (
        StatusCode::OK,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_JSON),
        )],
        buf,
    )
        .into_response();
    append_response_metadata(response.headers_mut(), &metadata, false);
    response
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
    let body = serde_json::to_vec(&error_payload(status)).unwrap_or_default();
    let http_status = StatusCode::from_u16(http_status(status.code()))
        .unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let mut response = (
        http_status,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_JSON),
        )],
        body,
    )
        .into_response();
    append_response_metadata(response.headers_mut(), status.metadata(), true);
    response
}

fn error_response_with_limit(status: &Status, max_response_bytes: usize) -> Response {
    let mut code = status.code();
    let mut body = serde_json::to_vec(&error_payload(status)).unwrap_or_default();
    let mut preserve_metadata = true;
    if max_response_bytes > 0 && body.len() > max_response_bytes {
        code = Code::ResourceExhausted;
        body = serde_json::to_vec(&json!({"code": "resource_exhausted"})).unwrap_or_default();
        preserve_metadata = false;
        if body.len() > max_response_bytes {
            body.clear();
        }
    }
    let http_status =
        StatusCode::from_u16(http_status(code)).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let mut response = (
        http_status,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_JSON),
        )],
        body,
    )
        .into_response();
    if preserve_metadata {
        append_response_metadata(response.headers_mut(), status.metadata(), true);
    }
    response
}

fn unsupported_media_type_response(message: &str, max_response_bytes: usize) -> Response {
    let status = Status::invalid_argument(message);
    let mut body = serde_json::to_vec(&error_payload(&status)).unwrap_or_default();
    if max_response_bytes > 0 && body.len() > max_response_bytes {
        body.clear();
    }
    (
        StatusCode::UNSUPPORTED_MEDIA_TYPE,
        [(
            header::CONTENT_TYPE,
            HeaderValue::from_static(CONTENT_TYPE_JSON),
        )],
        body,
    )
        .into_response()
}

fn append_response_metadata(headers: &mut HeaderMap, metadata: &MetadataMap, trailers: bool) {
    for item in metadata.iter() {
        let (key, value) = match item {
            KeyAndValueRef::Ascii(key, value) => {
                let Ok(value) = value.to_str() else {
                    continue;
                };
                (key.as_str(), value.to_string())
            }
            KeyAndValueRef::Binary(key, value) => {
                let Ok(value) = value.to_bytes() else {
                    continue;
                };
                (
                    key.as_str(),
                    base64::engine::general_purpose::STANDARD_NO_PAD.encode(value),
                )
            }
        };
        if reserved_response_metadata(key) {
            continue;
        }
        let name = if trailers {
            format!("trailer-{key}")
        } else {
            key.to_string()
        };
        let (Ok(name), Ok(value)) = (
            HeaderName::from_bytes(name.as_bytes()),
            HeaderValue::from_str(&value),
        ) else {
            continue;
        };
        headers.append(name, value);
    }
}

fn connect_end_stream_metadata(metadata: &MetadataMap) -> BTreeMap<String, Vec<String>> {
    let mut output = BTreeMap::new();
    for item in metadata.iter() {
        let (key, value) = match item {
            KeyAndValueRef::Ascii(key, value) => {
                let Ok(value) = value.to_str() else {
                    continue;
                };
                (key.as_str(), value.to_string())
            }
            KeyAndValueRef::Binary(key, value) => {
                let Ok(value) = value.to_bytes() else {
                    continue;
                };
                (
                    key.as_str(),
                    base64::engine::general_purpose::STANDARD_NO_PAD.encode(value),
                )
            }
        };
        if !reserved_response_metadata(key) {
            output
                .entry(key.to_string())
                .or_insert_with(Vec::new)
                .push(value);
        }
    }
    output
}

fn reserved_response_metadata(key: &str) -> bool {
    let key = key.to_ascii_lowercase();
    let logical_key = key.strip_suffix("-bin").unwrap_or(&key);
    logical_key.starts_with("grpc-")
        || logical_key.starts_with("connect-")
        || logical_key.starts_with("invariant-internal-")
        || logical_key.starts_with("x-invariant-internal-")
        || logical_key.starts_with("trailer-")
        || matches!(
            logical_key,
            "te" | "host"
                | "connection"
                | "keep-alive"
                | "proxy-connection"
                | "transfer-encoding"
                | "upgrade"
                | "content-length"
                | "content-type"
                | "content-encoding"
                | "accept-encoding"
                | "content-range"
                | "trailer"
        )
}

/// Extract `unknown field "..."` from protobuf JSON parse errors and surface
/// it as a structured `BadRequest` detail. Mirrors Go's
/// `invalidArgumentFromJSONError`.
fn invalid_argument_from_json_error(msg: &str) -> Status {
    let mut field = None;
    for (needle, terminator) in [
        ("unknown field \"", '"'),
        ("no field named \"", '"'),
        ("unrecognized field name '", '\''),
    ] {
        if let Some(start) = msg.find(needle) {
            let after = &msg[start + needle.len()..];
            if let Some(end) = after.find(terminator) {
                field = Some(after[..end].to_string());
                break;
            }
        }
    }
    if let Some(f) = field {
        return Status::with_error_details(
            Code::InvalidArgument,
            format!("proto: {msg}"),
            ErrorDetails::with_bad_request_violation(f, format!("proto: {msg}")),
        );
    }
    Status::invalid_argument(format!("proto: {msg}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;

    #[tokio::test]
    async fn projection_deadline_is_checked_before_poll_and_after_completion() {
        let projection = ProjectionContext::new(Some(Duration::from_millis(1)));
        tokio::time::sleep(Duration::from_millis(5)).await;
        let polled = Cell::new(false);
        let result = until_projection_deadline(&projection, async {
            polled.set(true);
            Ok(())
        })
        .await;
        assert_eq!(result.unwrap_err().code(), Code::DeadlineExceeded);
        assert!(!polled.get());

        let projection = ProjectionContext::new(Some(Duration::from_millis(1)));
        let result = until_projection_deadline(&projection, async {
            let finished_at = std::time::Instant::now() + Duration::from_millis(10);
            while std::time::Instant::now() < finished_at {
                std::hint::spin_loop();
            }
            Ok(())
        })
        .await;
        assert_eq!(result.unwrap_err().code(), Code::DeadlineExceeded);
        assert!(projection.is_cancelled());
    }

    #[test]
    fn connect_timeout_requires_a_positive_unpadded_ascii_integer() {
        for invalid in [
            b"0".as_slice(),
            b"-1",
            b"+1",
            b" 1",
            b"1 ",
            b"1.0",
            b"abc",
            b"12345678901",
        ] {
            let mut headers = HeaderMap::new();
            headers.insert(
                CONNECT_TIMEOUT_HEADER,
                HeaderValue::from_bytes(invalid).unwrap(),
            );
            assert_eq!(
                parse_connect_timeout(&headers).unwrap_err().code(),
                Code::InvalidArgument,
                "{invalid:?}"
            );
        }

        let mut headers = HeaderMap::new();
        headers.insert(CONNECT_TIMEOUT_HEADER, HeaderValue::from_static("1"));
        assert_eq!(
            parse_connect_timeout(&headers).unwrap(),
            Some(Duration::from_millis(1))
        );
        headers.insert(
            CONNECT_TIMEOUT_HEADER,
            HeaderValue::from_static("9999999999"),
        );
        assert_eq!(
            parse_connect_timeout(&headers).unwrap(),
            Some(Duration::from_millis(9_999_999_999))
        );
    }
}
