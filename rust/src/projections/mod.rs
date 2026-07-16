//! Projections — wire surfaces that all sit on the same `Server` core.

pub mod cli;
pub mod grpc;
pub mod http;
pub(crate) mod http_client;
pub mod mcp;
pub mod serve;

pub use cli::cli_write;
pub use http::{http_router, serve_http};
pub use mcp::{MCP_PROTOCOL_VERSION, mcp_dispatch, serve_mcp_stdio};
pub use serve::{Projection, ServeError, serve};
