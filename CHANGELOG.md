# Changelog

All notable changes to this project are documented here. Versions track all
three implementations (Go, Python, Rust) in lockstep so consumers pinning to
a tag get the same feature set across languages.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
the project is pre-1.0 so 0.x.y minor bumps may include additive API changes,
but never silent behaviour regressions.

## v0.2.6 — 2026-06-01

### Added

- **`Server.use_http_query_provider` for `connect_http` (Python).** Symmetric to
  `use_http_header_provider`, but injects query-string parameters into each
  outbound request — for venues that authenticate via the query string (a plain
  API key, or an HMAC signature + timestamp signed over the request). The
  provider sees the fully-built request (existing query + body) so it can sign
  over it, and is re-run per retry so signatures/timestamps stay fresh. Both
  providers may be set at once (e.g. an API-key header + a signed query param).
  `HTTPQueryProvider` is exported.

## v0.2.5 — 2026-06-01

### Added

- **`Server.use_http_response_observer` for `connect_http` (Python).** An
  optional observer is called once per successful outbound response with the
  raw, undecoded body bytes (an `OutboundHTTPResponse`) before they are parsed
  into the typed message — so callers can archive the verbatim payload (e.g. a
  raw/bronze data tier) independent of what the response message models. The
  observer is best-effort: its return value is ignored and exceptions are
  suppressed so it can never break the call path. `OutboundHTTPResponse` and
  `HTTPResponseObserver` are exported alongside the existing
  `OutboundHTTPRequest` / `HTTPHeaderProvider`.

## v0.2.4 — 2026-06-01

### Changed

- **`connect_http` honors `json_name` on outbound requests (Python).** The
  request body and query parameters now serialize through the default proto3
  JSON mapping (which respects an explicit `json_name` and lowerCamelCases
  otherwise) instead of forcing raw proto field names. `google.api.http` path
  and `body` selectors still reference proto field names per the spec — they
  are translated to the corresponding JSON keys once at bind time, so
  `/v1/users/{user_id}` keeps working while a field declared
  `string time_in_force = 2 [json_name = "timeInForce"];` is sent as
  `timeInForce`. Single-word fields are unaffected. Set an explicit `json_name`
  to pin any wire key (e.g. snake_case for venues that expect it).

## v0.2.3 — 2026-05-24

### Added

- **Per-method body caps via `Server.ConfigureMethod` (Go).** New
  `MethodConfig{MaxUnaryRequestBytes, MaxStreamRequestBytes}` type lets
  one outlier RPC (Upload, BulkImport) accept large bodies while the
  rest of the service stays tightly capped. Zero-valued fields inherit
  the server-level setting; non-zero override per method.

  Use case: ghdrive's Upload legitimately needs 1 GiB, but ListDir
  should reject anything over a few KiB. Before, you had to either
  raise the server-wide cap (and lose the safety on every other RPC)
  or write custom middleware. Now:
  `srv.ConfigureMethod("/pkg.Service/Upload", invariant.MethodConfig{MaxUnaryRequestBytes: 1 << 30})`
  and the rest of the surface stays bounded.

### Changed

- **Auto-matcher now includes server-streaming methods (Go).**
  `Server.Register(servicer, ...)` previously skipped streaming methods
  during the service-name auto-discovery, forcing callers to explicitly
  name the service even when one method was streaming. Now a servicer
  with any mix of unary + server-streaming handlers auto-matches.
  Client-streaming stays excluded (the framework doesn't project it).

  No effect on services that were already passing the service name
  explicitly. The change just removes a confusing inconsistency
  where adding a `Download(req, stream) error` method to a previously
  auto-matched servicer would silently break registration.

### Internal

- New test `TestHTTPUnaryConfigureMethodPerMethodCap` in `hardening_test.go`.
- Python and Rust parity for `MethodConfig` is a follow-up release.

## v0.2.2 — 2026-05-21

### Added

- **Rust gRPC honors the `grpc-timeout` header.** Our hand-rolled gRPC path
  (we decode frames ourselves for descriptor-driven dispatch) now parses the
  gRPC-spec deadline format `<digits><unit>` (`n`/`u`/`m`/`S`/`M`/`H`) and
  wraps invocation in `tokio::time::timeout`. Unary returns
  `deadline_exceeded`; streaming surfaces the same code in the HTTP/2 trailer.
  Closes the only gRPC-first gap that wasn't auto-handled by tonic.
- Parser unit-tested for all six gRPC time units plus the malformed-header
  fallback (treat as "no deadline" rather than fail the request).

### Documented

- `rust/src/validation.rs` now openly states why Rust doesn't ship a built-in
  protovalidate interceptor: the `prost-protovalidate 0.3` crate would cascade
  prost 0.13→0.14 / prost-reflect 0.14→0.16 / tonic 0.12→0.14 / axum 0.7→0.8,
  which isn't "thin and simple". Users compose their own validation
  interceptor via `use_interceptor` / `use_stream_interceptor` until the
  Rust proto-validation ecosystem stabilises on prost 0.14.

## v0.2.1 — 2026-05-21

### Added

- **Configurable HTTP body-size caps** across Go / Python / Rust. Defaults
  stay at 16 MiB; applications with legitimate large-upload needs raise the
  cap per-server instead of writing custom middleware.
  - Go: `Server.SetMaxUnaryRequestBytes(n)` / `Server.SetMaxStreamRequestBytes(n)`
  - Python: `Server.set_max_unary_request_bytes(n)` / `set_max_stream_request_bytes(n)`
  - Rust: `Server::set_max_unary_request_bytes(n)` / `set_max_stream_request_bytes(n)`
    plus `max_*_request_bytes()` getters for introspection
  - Passing 0 resets to the 16 MiB default in all three languages.

## v0.2.0 — 2026-05-20

### Added

- **Rust implementation** (new). Descriptor-driven dispatch via prost-reflect,
  no per-service codegen. Same shape-mirror philosophy as Go and Python.
- **All four projections in Rust**: unary + server-streaming RPCs over
  Connect (`application/json`, `application/proto`, `application/connect+json`,
  `application/connect+proto`), gRPC (descriptor-driven via tonic 0.12's
  `Routes::from(axum::Router)`), MCP (stdio + HTTP transport), CLI.
- **gRPC reflection auto-registered** in all three languages, including the
  new Rust implementation.
- **gRPC-first stance made explicit** in the README — every projection is
  documented as a translation of the gRPC service it projects.
- **Streaming server-streaming RPCs** previously projected across Go and
  Python now also work end-to-end in Rust, including HTTP/2 trailers for
  the gRPC status code on stream termination.
- **Production hardening across all three languages**:
  panic/exception recovery in dispatch core, 16 MiB body caps on Connect +
  gRPC, Connect-Timeout-Ms on every HTTP path, MCP stdio cancellation via
  `notifications/cancelled`, multi-projection serve with graceful shutdown.
- **`_meta.streaming` tool catalog annotation** so MCP clients can render
  streaming tools differently from unary ones.
- **Flox manifest** ships the Rust toolchain (`rustc`, `cargo`, `clippy`,
  `rustfmt`, `rust-analyzer`) so contributors get parity with the Go and
  Python toolchains via `flox activate`.

### Benchmarks (Intel Xeon E5-2696 v4, Linux)

| Path                        | Go      | Python    | Rust    |
|-----------------------------|---------|-----------|---------|
| Direct `invoke` (unary)     | 2.9 µs  | 2.0 µs    | 0.8 µs  |
| HTTP JSON unary roundtrip   | 282 µs  | 1677 µs   | 206 µs  |
| HTTP binary proto unary     | 261 µs  | 1641 µs   | 199 µs  |
| gRPC unary                  | 318 µs  | 509 µs    | (n/a)   |

## v0.1.0 — 2026-04-15

Initial release. Go and Python implementations of the descriptor-driven
projection framework. Connect (HTTP) + gRPC + MCP (stdio) + CLI surfaces.
Streaming RPCs, MCP HTTP transport, hardening, and production-server
coverage all landed in this release. See git history before tagging for
the development arc.

[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/
