//! Caller-owned Reqwest transport for unary remote HTTP/Connect projections.

use crate::errors::connect_code;
use crate::server::{ProjectionContext, Server, UnaryProjectionHandler};
use base64::Engine;
use futures::StreamExt;
use prost::Message;
use prost_reflect::{DynamicMessage, MessageDescriptor};
use reqwest::{Client, Url, header};
use serde::Deserialize;
use std::sync::Arc;
use std::time::Duration;
use tonic::codegen::Bytes;
use tonic::metadata::{Ascii, Binary, MetadataKey, MetadataMap, MetadataValue};
use tonic::{Code, Request, Response, Status};

pub(crate) fn remote_unary_handler(
    server: Server,
    client: Client,
    base_url: Url,
    service: String,
    method: String,
    output: MessageDescriptor,
) -> UnaryProjectionHandler {
    Arc::new(move |request: Request<DynamicMessage>| {
        let server = server.clone();
        let client = client.clone();
        let base_url = base_url.clone();
        let service = service.clone();
        let method = method.clone();
        let output = output.clone();
        Box::pin(async move {
            call_remote_http(
                &server, &client, base_url, &service, &method, output, request,
            )
            .await
        })
    })
}

async fn call_remote_http(
    server: &Server,
    client: &Client,
    mut url: Url,
    service: &str,
    method: &str,
    output: MessageDescriptor,
    request: Request<DynamicMessage>,
) -> Result<Response<DynamicMessage>, Status> {
    let limits = server.http_limits_for_method(service, method);
    let (metadata, extensions, message) = request.into_parts();
    let body = message.encode_to_vec();
    if body.len() > limits.unary_request {
        return Err(Status::resource_exhausted(format!(
            "remote HTTP request exceeds {} byte limit",
            limits.unary_request
        )));
    }

    let base_path = url.path().trim_end_matches('/');
    url.set_path(&format!("{base_path}/{service}/{method}"));
    url.set_query(None);
    url.set_fragment(None);

    let timeout = extensions
        .get::<ProjectionContext>()
        .and_then(ProjectionContext::remaining)
        .or_else(|| metadata.get("grpc-timeout").and_then(parse_grpc_timeout));
    let mut outbound = client
        .post(url)
        .header(header::CONTENT_TYPE, "application/proto")
        .header(header::ACCEPT, "application/proto")
        .headers(outbound_headers(&metadata))
        .body(body);
    if let Some(timeout) = timeout {
        outbound = outbound
            .timeout(timeout)
            .header("connect-timeout-ms", timeout.as_millis().max(1).to_string());
    }

    let response = outbound.send().await.map_err(reqwest_status)?;
    let status_code = response.status();
    let content_type = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .split(';')
        .next()
        .unwrap_or_default()
        .trim()
        .to_string();
    let response_metadata = response_metadata(response.headers());
    let bytes = collect_bounded(response, limits.unary_response).await?;

    if !status_code.is_success() {
        return Err(parse_connect_error(status_code, &bytes, response_metadata));
    }
    let message = if content_type == "application/proto" {
        DynamicMessage::decode(output, bytes.as_ref())
            .map_err(|error| Status::internal(format!("decode remote protobuf: {error}")))?
    } else if content_type == "application/json" {
        let mut deserializer = serde_json::Deserializer::from_slice(&bytes);
        DynamicMessage::deserialize_with_options(
            output,
            &mut deserializer,
            &prost_reflect::DeserializeOptions::new(),
        )
        .map_err(|error| Status::internal(format!("decode remote protobuf JSON: {error}")))?
    } else {
        return Err(Status::internal(format!(
            "remote HTTP response has unsupported content type {content_type:?}"
        )));
    };
    Ok(Response::from_parts(
        response_metadata,
        message,
        http::Extensions::new(),
    ))
}

async fn collect_bounded(response: reqwest::Response, max: usize) -> Result<Bytes, Status> {
    if response
        .content_length()
        .is_some_and(|content_length| content_length > max as u64)
    {
        return Err(Status::resource_exhausted(format!(
            "remote HTTP response exceeds {max} byte limit"
        )));
    }
    let mut stream = response.bytes_stream();
    let mut output = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(reqwest_status)?;
        if output.len().saturating_add(chunk.len()) > max {
            return Err(Status::resource_exhausted(format!(
                "remote HTTP response exceeds {max} byte limit"
            )));
        }
        output.extend_from_slice(&chunk);
    }
    Ok(Bytes::from(output))
}

fn outbound_headers(metadata: &MetadataMap) -> header::HeaderMap {
    let mut headers = header::HeaderMap::new();
    for key in ["traceparent", "tracestate", "baggage", "x-request-id"] {
        for value in metadata.get_all(key).iter() {
            let Ok(value) = value.to_str() else {
                continue;
            };
            let Ok(value) = header::HeaderValue::from_str(value) else {
                continue;
            };
            headers.append(header::HeaderName::from_static(key), value);
        }
    }
    headers
}

fn response_metadata(headers: &header::HeaderMap) -> MetadataMap {
    let mut metadata = MetadataMap::new();
    for (name, value) in headers {
        let mut key = name.as_str();
        if let Some(trailer) = key.strip_prefix("trailer-") {
            key = trailer;
        }
        if reserved_transport_header(key) {
            continue;
        }
        if key.ends_with("-bin") {
            let Ok(value) = value.to_str() else {
                continue;
            };
            let decoded = base64::engine::general_purpose::STANDARD_NO_PAD
                .decode(value)
                .or_else(|_| base64::engine::general_purpose::STANDARD.decode(value));
            if let (Ok(key), Ok(value)) =
                (MetadataKey::<Binary>::from_bytes(key.as_bytes()), decoded)
            {
                metadata.append_bin(key, MetadataValue::from_bytes(&value));
            }
        } else if let Ok(value) = value.to_str()
            && let (Ok(key), Ok(value)) = (
                MetadataKey::<Ascii>::from_bytes(key.as_bytes()),
                value.parse(),
            )
        {
            metadata.append(key, value);
        }
    }
    metadata
}

fn reserved_transport_header(key: &str) -> bool {
    key.starts_with("grpc-")
        || key.starts_with("connect-")
        || matches!(
            key,
            "content-length"
                | "content-type"
                | "content-encoding"
                | "accept"
                | "accept-encoding"
                | "host"
                | "connection"
                | "keep-alive"
                | "proxy-connection"
                | "te"
                | "trailer"
                | "transfer-encoding"
                | "upgrade"
        )
}

fn parse_grpc_timeout(value: &MetadataValue<tonic::metadata::Ascii>) -> Option<Duration> {
    let value = value.to_str().ok()?;
    let (number, unit) = value.split_at(value.len().checked_sub(1)?);
    let number = number.parse::<u64>().ok()?;
    match unit {
        "H" => Some(Duration::from_secs(number.saturating_mul(60 * 60))),
        "M" => Some(Duration::from_secs(number.saturating_mul(60))),
        "S" => Some(Duration::from_secs(number)),
        "m" => Some(Duration::from_millis(number)),
        "u" => Some(Duration::from_micros(number)),
        "n" => Some(Duration::from_nanos(number)),
        _ => None,
    }
}

fn reqwest_status(error: reqwest::Error) -> Status {
    if error.is_timeout() {
        Status::deadline_exceeded(format!("remote HTTP deadline exceeded: {error}"))
    } else if error.is_connect() {
        Status::unavailable(format!("remote HTTP connection failed: {error}"))
    } else {
        Status::unknown(format!("remote HTTP request failed: {error}"))
    }
}

#[derive(Deserialize)]
struct ConnectError {
    code: String,
    message: String,
    #[serde(default)]
    details: Vec<ConnectDetail>,
}

#[derive(Deserialize)]
struct ConnectDetail {
    #[serde(rename = "type")]
    type_name: String,
    value: String,
}

fn parse_connect_error(
    http_status: reqwest::StatusCode,
    body: &[u8],
    metadata: MetadataMap,
) -> Status {
    let Ok(error) = serde_json::from_slice::<ConnectError>(body) else {
        return Status::with_metadata(
            code_for_http_status(http_status),
            String::from_utf8_lossy(body).into_owned(),
            metadata,
        );
    };
    let code = code_from_connect(&error.code);
    let details = error
        .details
        .into_iter()
        .filter_map(|detail| {
            let value = base64::engine::general_purpose::STANDARD_NO_PAD
                .decode(detail.value)
                .ok()?;
            Some(prost_types::Any {
                type_url: format!("type.googleapis.com/{}", detail.type_name),
                value,
            })
        })
        .collect::<Vec<_>>();
    if details.is_empty() {
        return Status::with_metadata(code, error.message, metadata);
    }
    let rich_status = tonic_types::pb::Status {
        code: code as i32,
        message: error.message.clone(),
        details,
    }
    .encode_to_vec();
    Status::with_details_and_metadata(code, error.message, Bytes::from(rich_status), metadata)
}

fn code_from_connect(code: &str) -> Code {
    [
        Code::Ok,
        Code::Cancelled,
        Code::Unknown,
        Code::InvalidArgument,
        Code::DeadlineExceeded,
        Code::NotFound,
        Code::AlreadyExists,
        Code::PermissionDenied,
        Code::ResourceExhausted,
        Code::FailedPrecondition,
        Code::Aborted,
        Code::OutOfRange,
        Code::Unimplemented,
        Code::Internal,
        Code::Unavailable,
        Code::DataLoss,
        Code::Unauthenticated,
    ]
    .into_iter()
    .find(|candidate| connect_code(*candidate) == code)
    .unwrap_or(Code::Unknown)
}

fn code_for_http_status(status: reqwest::StatusCode) -> Code {
    match status.as_u16() {
        400 => Code::InvalidArgument,
        401 => Code::Unauthenticated,
        403 => Code::PermissionDenied,
        404 => Code::NotFound,
        409 => Code::Aborted,
        429 => Code::ResourceExhausted,
        499 => Code::Cancelled,
        501 => Code::Unimplemented,
        503 => Code::Unavailable,
        504 => Code::DeadlineExceeded,
        _ => Code::Unknown,
    }
}
