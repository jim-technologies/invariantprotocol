//! Connect error rendering for canonical [`tonic::Status`] values.

use base64::Engine;
use prost::Message;
use serde::Serialize;
use tonic::{Code, Status};

/// Connect-style error payload. Native gRPC keeps the complete tonic status;
/// rich details are rendered in Connect's lossless type/value wire shape.
#[derive(Debug, Clone, Serialize)]
pub struct ErrorPayload {
    pub code: &'static str,
    pub message: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub details: Vec<serde_json::Value>,
}

pub fn error_payload(status: &Status) -> ErrorPayload {
    let details = tonic_types::pb::Status::decode(status.details())
        .map(|status| {
            status
                .details
                .into_iter()
                .map(|detail| {
                    let type_name = detail
                        .type_url
                        .rsplit_once('/')
                        .map_or(detail.type_url.as_str(), |(_, name)| name);
                    serde_json::json!({
                        "type": type_name,
                        "value": base64::engine::general_purpose::STANDARD_NO_PAD
                            .encode(detail.value),
                    })
                })
                .collect()
        })
        .unwrap_or_default();
    ErrorPayload {
        code: connect_code(status.code()),
        message: status.message().to_string(),
        details,
    }
}

pub fn connect_code(code: Code) -> &'static str {
    match code {
        Code::Ok => "ok",
        Code::Cancelled => "canceled",
        Code::Unknown => "unknown",
        Code::InvalidArgument => "invalid_argument",
        Code::DeadlineExceeded => "deadline_exceeded",
        Code::NotFound => "not_found",
        Code::AlreadyExists => "already_exists",
        Code::PermissionDenied => "permission_denied",
        Code::ResourceExhausted => "resource_exhausted",
        Code::FailedPrecondition => "failed_precondition",
        Code::Aborted => "aborted",
        Code::OutOfRange => "out_of_range",
        Code::Unimplemented => "unimplemented",
        Code::Internal => "internal",
        Code::Unavailable => "unavailable",
        Code::DataLoss => "data_loss",
        Code::Unauthenticated => "unauthenticated",
    }
}

pub fn http_status(code: Code) -> u16 {
    match code {
        Code::Ok => 200,
        Code::Cancelled => 499,
        Code::Unknown => 500,
        Code::InvalidArgument | Code::FailedPrecondition | Code::OutOfRange => 400,
        Code::DeadlineExceeded => 504,
        Code::NotFound => 404,
        Code::AlreadyExists | Code::Aborted => 409,
        Code::PermissionDenied => 403,
        Code::ResourceExhausted => 429,
        Code::Unimplemented => 501,
        Code::Internal | Code::DataLoss => 500,
        Code::Unavailable => 503,
        Code::Unauthenticated => 401,
    }
}
