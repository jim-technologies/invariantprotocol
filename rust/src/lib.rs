//! # invariant-protocol
//!
//! One proto descriptor in → Connect / gRPC / MCP / CLI projections out.
//!
//! Rust mirror of the Go and Python implementations: same API in spirit
//! ([`Server::from_descriptor`], [`Server::register_unary`], `serve`,
//! `invoke`, `use_*`), idiomatic in form. We do *not* depend on the
//! `connectrpc` crate because its model is per-service codegen and
//! generated traits — that's incompatible with our descriptor-driven
//! runtime dispatch. The Connect protocol is hand-rolled on top of
//! `axum` / `tower`, exactly the same way the Go and Python ports do it.
//!
//! ## API parity matrix
//!
//! | Concept            | Go                                | Python                          | Rust                                |
//! |--------------------|-----------------------------------|---------------------------------|-------------------------------------|
//! | Construct          | `ServerFromDescriptor(path)`     | `Server.from_descriptor(path)` | `Server::from_descriptor(path)`     |
//! | Register unary     | `Register(servicer)` (reflection) | `register(servicer)` (reflection) | `register_unary("Name", fn)`     |
//! | Register stream    | as above                          | as above                        | `register_stream("Name", fn)`       |
//! | Unary interceptor  | `Use(fn)`                         | `use(fn)`                       | `use_interceptor(fn)`               |
//! | Stream interceptor | `UseStream(fn)`                   | `use_stream(fn)`                | `use_stream_interceptor(fn)`        |
//! | Invoke             | `Invoke(ctx, name, req)`          | `invoke(name, req)`             | `invoke(name, req).await`           |
//! | Tool catalog       | `ToolCatalog()`                   | `tool_catalog()`                | `tool_catalog()`                    |
//!
//! Rust's lack of runtime method-name reflection makes per-method registration
//! explicit. The trade-off is named: Go/Python infer methods from the servicer
//! struct/class; Rust users state the mapping. Every other surface is
//! shape-identical.

pub mod descriptor;
pub mod errors;
pub mod projections;
pub mod schema;
pub mod server;
pub mod stream;
pub mod validation;

pub use descriptor::{MethodInfo, ParsedDescriptor, ServiceInfo};
pub use errors::{Code, Status};
pub use server::{Server, ServerCallInfo, Tool};
pub use stream::ServerStreamTx;

#[doc(hidden)]
pub use prost_reflect::{DescriptorPool, DynamicMessage, MessageDescriptor};
