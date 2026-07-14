//! Validation hook — Rust intentionally ships **no** built-in
//! `protovalidate` interceptor.
//!
//! Why: the Rust ecosystem still has no mature, official Buf protovalidate
//! runtime. The official `protovalidate-rust` crate remains a placeholder,
//! while current third-party implementations make different code-generation,
//! reflection, and CEL tradeoffs. Invariant should not select one of those
//! policies for every user. Users wanting `buf.validate` enforcement compose
//! it via [`Server::use_interceptor`] / [`Server::use_stream_interceptor`]
//! against the validator that fits their application.
//!
//! Example with a hand-rolled validator (a real codebase would call into
//! `prost-protovalidate` or similar):
//!
//! ```ignore
//! use invariant::server::UnaryInterceptor;
//! use std::sync::Arc;
//!
//! fn my_validator() -> UnaryInterceptor {
//!     Arc::new(|req, _info, next| {
//!         Box::pin(async move {
//!             // user-side validation against `req: DynamicMessage`
//!             next(req).await
//!         })
//!     })
//! }
//!
//! server.use_interceptor(my_validator());
//! ```
//!
//! We can revisit a first-class wrapper when the official Buf runtime is
//! production-ready. For now: explicit, user-side.
