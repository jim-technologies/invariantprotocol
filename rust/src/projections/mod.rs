//! Projections — wire surfaces that all sit on the same `Server` core.

pub mod cli;
pub mod grpc;
pub mod http;
pub mod mcp;
pub mod serve;

pub use cli::cli_write;
pub use grpc::{grpc_router, grpc_routes, serve_grpc};
pub use http::{http_router, serve_http};
pub use mcp::{mcp_dispatch, serve_mcp_stdio, MCP_PROTOCOL_VERSION};
pub use serve::{serve, Projection, ServeError};
