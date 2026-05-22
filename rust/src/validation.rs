//! Validation hook — Rust intentionally ships **no** built-in
//! `protovalidate` interceptor.
//!
//! Why: the reflection-based Rust validator crates require `prost-reflect
//! 0.16+` which pulls `prost 0.14` and forces a cascade through `tonic 0.14`
//! and `axum 0.8`. That dep churn isn't "thin and simple", and the gRPC and
//! Connect protocols themselves don't depend on validation. Users wanting
//! `buf.validate` enforcement compose it via [`Server::use_interceptor`] /
//! [`Server::use_stream_interceptor`] against any validator they prefer.
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
//! When the dep landscape stabilises around prost 0.14 we'll revisit
//! shipping a first-class wrapper here. For now: explicit, user-side.
