//! gRPC projection — descriptor-driven dispatch over tonic's transport.
//!
//! We build a regular `axum::Router` mapping each
//! registered tool's `/{package.Service}/{Method}` to a handler that decodes
//! the gRPC length-prefixed frame, dispatches via [`Server::invoke`] (or
//! [`Server::invoke_stream`]), and encodes the response back into the gRPC
//! wire format. Then `tonic::service::Routes::from(axum::Router)` lifts the
//! axum router into tonic's transport so we still get HTTP/2 prior-knowledge
//! negotiation for gRPC clients (Connect-Go, grpcurl, Buf Studio).
//!
//! Why not tonic codegen: it requires per-service `NamedService::NAME` at
//! compile time. The framework's stance is descriptor-driven runtime
//! dispatch — generated traits would contradict that. Hand-rolling the
//! frame codec is ~30 LOC; tonic handles the transport.
//!
//! No middleware / interceptor layer is added on this projection — users
//! compose their own via `Server::use_interceptor` / `use_stream_interceptor`
//! (which apply across all projections), or via tower layers added by the
//! caller of `serve_grpc` if they want gRPC-specific layers.

use crate::errors::{Code, Status};
use crate::server::{Server, Tool};
use axum::Router;
use axum::body::Body;
use axum::extract::{Request, State};
use axum::http::{HeaderMap, HeaderName, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use futures::StreamExt;
use http_body_util::{BodyExt, StreamBody};
use prost::Message;
use prost_reflect::DynamicMessage;
use std::sync::Arc;
use tonic::service::Routes;

const CONTENT_TYPE_GRPC: &str = "application/grpc";
const CONTENT_TYPE_GRPC_PROTO: &str = "application/grpc+proto";
/// 16 MiB inbound cap — matches the Connect/HTTP unary cap. Hostile or buggy
/// clients can't stream a multi-gigabyte body and OOM us.
const GRPC_MAX_REQUEST_BYTES: usize = 16 * 1024 * 1024;

/// Build the gRPC routes (one POST per registered tool) as `tonic::Routes`
/// so the caller can mount them on tonic's transport via `add_routes`.
///
/// Internally this is an `axum::Router` lifted into `Routes` via the modern
/// `Routes::from(axum::Router)` conversion provided by tonic.
pub fn grpc_routes(server: Arc<Server>) -> Routes {
    Routes::from(grpc_router(server))
}

/// Build the underlying axum Router. Exposed for callers who want to mount
/// the gRPC paths directly inside their own axum app (HTTP/2 only).
pub fn grpc_router(server: Arc<Server>) -> Router {
    let mut router = Router::new();
    for tool in server.tools_snapshot() {
        let path = format!("/{}/{}", tool.service_full_name, tool.method_name);
        router = router.route(&path, post(grpc_handler));
    }
    router.with_state(server)
}

/// Serve gRPC on `port` until the future is dropped. Uses tonic's transport
/// for HTTP/2 prior-knowledge negotiation so any gRPC client just works.
/// gRPC reflection (`grpc.reflection.v1.ServerReflection`) is registered
/// automatically — same as Go/Python — so `grpcurl`, Buf Studio, and any
/// reflection-aware client work out of the box.
pub async fn serve_grpc(server: Arc<Server>, port: u16) -> Result<(), tonic::transport::Error> {
    let addr: std::net::SocketAddr = format!("[::]:{port}").parse().expect("valid socket addr");
    let reflection = build_reflection(&server).expect("reflection from FDS bytes");
    // Order matters: `add_routes` is on Server (consumes it and returns Router);
    // `add_service` is also on Router (chains more services). Reflection is a
    // named service, so it goes through the second call.
    tonic::transport::Server::builder()
        .add_routes(grpc_routes(server))
        .add_service(reflection)
        .serve(addr)
        .await
}

/// Build the gRPC reflection service from the registered descriptor's raw
/// FileDescriptorSet bytes. Mirrors Go's `reflection.Register(gs)` and
/// Python's `reflection.enable_server_reflection(...)`.
pub fn build_reflection(
    server: &Server,
) -> Result<
    tonic_reflection::server::v1::ServerReflectionServer<
        impl tonic_reflection::server::v1::ServerReflection,
    >,
    tonic_reflection::server::Error,
> {
    tonic_reflection::server::Builder::configure()
        .register_encoded_file_descriptor_set(&server.parsed().raw_fds)
        .build_v1()
}

// ---------- handler ----------

async fn grpc_handler(State(server): State<Arc<Server>>, req: Request) -> Response {
    let path = req.uri().path().to_string();
    let Some((service, method)) = path.trim_start_matches('/').split_once('/') else {
        return grpc_error(Code::Unimplemented, "unknown gRPC method");
    };
    let Some(tool) = server
        .tools_snapshot()
        .into_iter()
        .find(|t| t.service_full_name == service && t.method_name == method)
    else {
        return grpc_error(Code::Unimplemented, "unknown gRPC method");
    };

    // gRPC clients carry their deadline in the `grpc-timeout` header. We
    // parse it and apply via tokio::time::timeout so server-side work stops
    // when the client's deadline elapses — same semantics Go's grpc-go and
    // Python's grpcio give for free, and that our Connect path achieves via
    // `Connect-Timeout-Ms`.
    let timeout = parse_grpc_timeout(req.headers());
    let body_bytes = match read_body_capped(req).await {
        Ok(b) => b,
        Err(s) => return grpc_status_response(&s),
    };
    let dyn_req = match decode_frame(&body_bytes, &tool) {
        Ok(m) => m,
        Err(s) => return grpc_status_response(&s),
    };

    if tool.server_streaming {
        return grpc_stream_response(server, tool, dyn_req, timeout).await;
    }

    let invoke = server.invoke(&tool.name, dyn_req);
    let result = match timeout {
        Some(d) => match tokio::time::timeout(d, invoke).await {
            Ok(r) => r,
            Err(_) => Err(Status::deadline_exceeded(format!(
                "deadline exceeded after {}ms",
                d.as_millis()
            ))),
        },
        None => invoke.await,
    };
    match result {
        Ok(resp) => grpc_unary_response(&resp),
        Err(s) => grpc_status_response(&s),
    }
}

/// Test-only re-export of `parse_grpc_timeout` so unit tests can exercise
/// the parser without spinning up an HTTP/2 server.
#[doc(hidden)]
pub fn __test_parse_grpc_timeout(headers: &axum::http::HeaderMap) -> Option<std::time::Duration> {
    parse_grpc_timeout(headers)
}

/// Parse the gRPC `grpc-timeout` header per the gRPC HTTP/2 spec. Format is
/// `<positive integer><unit>` where unit is one of:
/// `n` nanoseconds, `u` microseconds, `m` milliseconds, `S` seconds,
/// `M` minutes, `H` hours. Returns `None` if absent or malformed (treat as
/// no deadline rather than fail the request).
fn parse_grpc_timeout(headers: &axum::http::HeaderMap) -> Option<std::time::Duration> {
    let raw = headers.get("grpc-timeout")?.to_str().ok()?;
    let raw = raw.trim();
    let (value, unit) = raw.split_at(raw.len().checked_sub(1)?);
    let value: u64 = value.parse().ok()?;
    let dur = match unit {
        "n" => std::time::Duration::from_nanos(value),
        "u" => std::time::Duration::from_micros(value),
        "m" => std::time::Duration::from_millis(value),
        "S" => std::time::Duration::from_secs(value),
        "M" => std::time::Duration::from_secs(value.checked_mul(60)?),
        "H" => std::time::Duration::from_secs(value.checked_mul(3600)?),
        _ => return None,
    };
    Some(dur)
}

/// Collect the request body, enforcing the 16 MiB cap so a hostile client
/// can't drive the server OOM. Returns `RESOURCE_EXHAUSTED` on overrun —
/// same code Go's `http.MaxBytesReader` maps to.
async fn read_body_capped(req: Request) -> Result<bytes::Bytes, Status> {
    let collected = req
        .into_body()
        .collect()
        .await
        .map_err(|e| Status::new(Code::Internal, format!("read body: {e}")))?;
    let bytes = collected.to_bytes();
    if bytes.len() > GRPC_MAX_REQUEST_BYTES {
        return Err(Status::resource_exhausted(format!(
            "request body exceeds {GRPC_MAX_REQUEST_BYTES} byte limit"
        )));
    }
    Ok(bytes)
}

async fn grpc_stream_response(
    server: Arc<Server>,
    tool: Arc<Tool>,
    dyn_req: DynamicMessage,
    timeout: Option<std::time::Duration>,
) -> Response {
    let mut stream = server.invoke_stream(&tool.name, dyn_req);
    let deadline = timeout.map(|d| tokio::time::Instant::now() + d);
    // gRPC requires status codes in HTTP/2 trailers, not headers. We yield
    // one `Frame::data` per message, then a final `Frame::trailers` carrying
    // `grpc-status` + (on error) `grpc-message`. axum's `Body::new` lets us
    // wrap any `http_body::Body`, so we drive it with `StreamBody`.
    let body_stream = async_stream::stream! {
        let mut grpc_status = 0i32; // Code::Ok
        let mut grpc_message: Option<String> = None;
        loop {
            let item = if let Some(d) = deadline {
                match tokio::time::timeout_at(d, stream.next()).await {
                    Ok(it) => it,
                    Err(_) => {
                        grpc_status = Code::DeadlineExceeded as i32;
                        grpc_message = Some(format!(
                            "deadline exceeded after {}ms",
                            timeout.unwrap_or_default().as_millis()
                        ));
                        break;
                    }
                }
            } else {
                stream.next().await
            };
            match item {
                None => break,
                Some(Ok(msg)) => {
                    let payload = msg.encode_to_vec();
                    yield Ok::<_, std::io::Error>(http_body::Frame::data(
                        bytes::Bytes::from(encode_frame(&payload)),
                    ));
                }
                Some(Err(s)) => {
                    grpc_status = s.code as i32;
                    grpc_message = Some(s.message.clone());
                    break;
                }
            }
        }
        let mut trailers = HeaderMap::new();
        if let Ok(v) = HeaderValue::from_str(&grpc_status.to_string()) {
            trailers.insert(HeaderName::from_static("grpc-status"), v);
        }
        if let Some(msg) = grpc_message
            && let Ok(v) = HeaderValue::from_str(&msg)
        {
            trailers.insert(HeaderName::from_static("grpc-message"), v);
        }
        yield Ok(http_body::Frame::trailers(trailers));
    };
    let body = Body::new(StreamBody::new(body_stream));
    Response::builder()
        .status(StatusCode::OK)
        .header(axum::http::header::CONTENT_TYPE, CONTENT_TYPE_GRPC)
        .body(body)
        .unwrap()
}

fn grpc_unary_response(msg: &DynamicMessage) -> Response {
    let payload = msg.encode_to_vec();
    let frame = encode_frame(&payload);
    let mut headers = HeaderMap::new();
    headers.insert(
        axum::http::header::CONTENT_TYPE,
        HeaderValue::from_static(CONTENT_TYPE_GRPC),
    );
    headers.insert("grpc-status", HeaderValue::from_static("0"));
    (StatusCode::OK, headers, frame).into_response()
}

fn grpc_status_response(s: &Status) -> Response {
    grpc_error(s.code, &s.message)
}

fn grpc_error(code: Code, message: &str) -> Response {
    let mut headers = HeaderMap::new();
    headers.insert(
        axum::http::header::CONTENT_TYPE,
        HeaderValue::from_static(CONTENT_TYPE_GRPC),
    );
    let code_num = (code as i32).to_string();
    if let Ok(v) = HeaderValue::from_str(&code_num) {
        headers.insert("grpc-status", v);
    }
    if let Ok(v) = HeaderValue::from_str(message) {
        headers.insert("grpc-message", v);
    }
    (StatusCode::OK, headers, ()).into_response()
}

fn decode_frame(buf: &[u8], tool: &Arc<Tool>) -> Result<DynamicMessage, Status> {
    if buf.len() < 5 {
        return Err(Status::invalid_argument("grpc frame too short"));
    }
    let compressed = buf[0] != 0;
    if compressed {
        return Err(Status::new(
            Code::Unimplemented,
            "compressed grpc frames not supported",
        ));
    }
    let size = u32::from_be_bytes([buf[1], buf[2], buf[3], buf[4]]) as usize;
    if buf.len() < 5 + size {
        return Err(Status::invalid_argument("grpc frame truncated"));
    }
    DynamicMessage::decode(tool.input_desc.clone(), &buf[5..5 + size])
        .map_err(|e| Status::invalid_argument(format!("decode grpc frame: {e}")))
}

fn encode_frame(payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(5 + payload.len());
    out.push(0); // not compressed
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

// Keep `application/grpc+proto` recognised for future content-type
// branching even though our decoder is content-agnostic.
const _: &str = CONTENT_TYPE_GRPC_PROTO;
