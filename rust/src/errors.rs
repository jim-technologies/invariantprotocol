//! gRPC-aligned error types. Mirrors `grpc.StatusCode` / `invariant.InvariantError`
//! semantically — same code enum, same Connect-style payload shape.
//!
//! Rust's `Status` here intentionally has the same name as `tonic::Status` so
//! handlers can return either without translation; we convert at projection
//! boundaries.

use serde::Serialize;
use std::fmt;

/// Canonical gRPC status code. Matches `google.rpc.Code` numerically and
/// `grpc::Code` from `tonic` 1:1 — we keep our own copy so the public API
/// doesn't transitively expose tonic to library consumers who don't need it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[repr(i32)]
pub enum Code {
    Ok = 0,
    Cancelled = 1,
    Unknown = 2,
    InvalidArgument = 3,
    DeadlineExceeded = 4,
    NotFound = 5,
    AlreadyExists = 6,
    PermissionDenied = 7,
    ResourceExhausted = 8,
    FailedPrecondition = 9,
    Aborted = 10,
    OutOfRange = 11,
    Unimplemented = 12,
    Internal = 13,
    Unavailable = 14,
    DataLoss = 15,
    Unauthenticated = 16,
}

impl Code {
    /// Lowercase Connect-style name (`"invalid_argument"`, `"not_found"`, ...).
    /// Wire format for HTTP/Connect error envelopes.
    pub fn as_connect_str(self) -> &'static str {
        match self {
            Code::Ok => "ok",
            Code::Cancelled => "cancelled",
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

    /// HTTP status mapping used by the Connect protocol. Mirrors the Go and
    /// Python tables so all three implementations map identically.
    pub fn http_status(self) -> u16 {
        match self {
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
}

/// Connect-style error payload. Lowercase code, optional details, no wrapper.
#[derive(Debug, Clone, Serialize)]
pub struct ErrorPayload {
    pub code: String,
    pub message: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub details: Vec<serde_json::Value>,
}

/// Framework error type. Same shape as Go's `status.Status` and Python's
/// `InvariantError`. Use [`Status::new`] for constructors with a single
/// code+message, or the helper builders below.
#[derive(Debug, Clone, thiserror::Error)]
#[error("{code:?}: {message}")]
pub struct Status {
    pub code: Code,
    pub message: String,
    pub details: Vec<serde_json::Value>,
}

impl Status {
    pub fn new<M: Into<String>>(code: Code, message: M) -> Self {
        Self {
            code,
            message: message.into(),
            details: Vec::new(),
        }
    }

    pub fn invalid_argument<M: Into<String>>(message: M) -> Self {
        Self::new(Code::InvalidArgument, message)
    }

    pub fn not_found<M: Into<String>>(message: M) -> Self {
        Self::new(Code::NotFound, message)
    }

    pub fn failed_precondition<M: Into<String>>(message: M) -> Self {
        Self::new(Code::FailedPrecondition, message)
    }

    pub fn internal<M: Into<String>>(message: M) -> Self {
        Self::new(Code::Internal, message)
    }

    pub fn resource_exhausted<M: Into<String>>(message: M) -> Self {
        Self::new(Code::ResourceExhausted, message)
    }

    pub fn deadline_exceeded<M: Into<String>>(message: M) -> Self {
        Self::new(Code::DeadlineExceeded, message)
    }

    /// Attach a structured detail object (matches `google.rpc.BadRequest` etc.).
    pub fn with_detail(mut self, detail: serde_json::Value) -> Self {
        self.details.push(detail);
        self
    }

    /// Render as a Connect-style payload — used by the HTTP projection and
    /// returned from `tool_catalog` errors verbatim.
    pub fn to_payload(&self) -> ErrorPayload {
        ErrorPayload {
            code: self.code.as_connect_str().to_string(),
            message: self.message.clone(),
            details: self.details.clone(),
        }
    }
}

impl From<prost::DecodeError> for Status {
    fn from(err: prost::DecodeError) -> Self {
        Status::invalid_argument(format!("decode proto: {err}"))
    }
}

impl From<prost::EncodeError> for Status {
    fn from(err: prost::EncodeError) -> Self {
        Status::internal(format!("encode proto: {err}"))
    }
}

impl From<serde_json::Error> for Status {
    fn from(err: serde_json::Error) -> Self {
        Status::invalid_argument(format!("json: {err}"))
    }
}

impl From<std::io::Error> for Status {
    fn from(err: std::io::Error) -> Self {
        Status::internal(format!("io: {err}"))
    }
}

impl fmt::Display for ErrorPayload {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}
