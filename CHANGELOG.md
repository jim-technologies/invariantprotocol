# Changelog

All notable changes to this project are documented here. Versions are tagged
repo-wide; entries call out implementation scope when a feature has not reached
all four implementations yet.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
the project is pre-1.0 so 0.x.y minor bumps may include additive API changes,
but never silent behaviour regressions.

## v0.5.0 — 2026-07-14

Package versions in this repository snapshot are Go `v0.3.1`, Python `0.4.0`,
Rust `0.3.0`, and TypeScript/npm `0.4.1`.

### Added

- **Generated gRPC registration is now the primary Go model.** `Server`
  implements `grpc.ServiceRegistrar`, accepts normal generated
  `Register<Service>Server` calls, retains typed service implementations for
  every projection, and exposes the native gRPC lifecycle directly.
- **Standard gRPC middleware and semantics across Go projections.** Shared
  unary and stream middleware uses gRPC interceptor types with typed generated
  messages, full method names, metadata, rich statuses, deadlines, and
  cancellation preserved across native gRPC and HTTP/Connect projections.
- **Complete native gRPC regression coverage.** Tests cover generated clients,
  unary and server streaming, interceptor ordering and single execution,
  metadata and trailers, rich status details, cancellation, message limits,
  registration freeze, and graceful shutdown.
- **Dependency automation for every package ecosystem.** Dependabot now covers
  both Go modules, Python/uv, Rust/Cargo, both npm package locations, and GitHub
  Actions.

### Changed

- **Current language and build ecosystems.** The reproducible Flox environment
  now uses Python 3.14, Go 1.26, Node 26, Rust 1.95 with edition 2024, GCC 16,
  Buf 1.71, and the latest compatible linting and generation tools available
  in the catalog.
- **Runtime dependencies were refreshed throughout.** Notable upgrades include
  gRPC Go 1.82, protobuf Python 7.35, grpcio 1.82, TypeScript 7, tonic/prost
  0.14, axum 0.8, and reqwest 0.13, with regenerated language stubs and lock
  files.
- **Descriptor generation follows Buf 1.71 defaults.** Source information is
  retained by default, validation stubs are regenerated deterministically from
  the built descriptor, and generated-file verification covers every emitted
  artifact.
- **HTTP/Connect bounds both request and response messages in Go.** Unary and
  streaming limits remain independent of native protobuf gRPC limits, and
  streaming limits apply per message.

### Fixed

- **Native Go unary interceptors now execute exactly once.** The generated-gRPC
  handler path invokes `grpc.UnaryServerInterceptor` using the standard typed
  terminal handler and correct `grpc.UnaryServerInfo` instead of bypassing it.

### Breaking

- Python now requires Python 3.14 or newer.
- Go now requires Go 1.26.4 or newer.
- Rust now requires Rust 1.95 or newer and uses edition 2024. Public tonic,
  prost, and axum ecosystem types follow their current major versions.

## v0.4.0 — 2026-07-07

### Added

- **TypeScript runtime.** Added a descriptor-driven Node package under
  `typescript/` using Protobuf-ES dynamic descriptors. It supports local
  servicer registration, unary and server-streaming `invoke`, unary and stream
  interceptors, JSON Schema/tool catalogs, CLI helpers, MCP dispatch including
  `POST /mcp`, remote gRPC proxying, remote HTTP proxying, Connect-ES powered
  HTTP/Connect routes, and grpc-js serving with reflection.
- **Default `make help` target.** The top-level Makefile now follows the
  project convention: plain `make` and `make help` list available
  targets.
- **Python `invariant-check-proto-comments` CLI.** Downstream services can run
  it against `descriptor.binpb` in CI to enforce comments on services, RPCs,
  messages, fields, enums, and enum values before projecting them into MCP,
  CLI, HTTP, or gRPC catalogs.

### Documented

- **Descriptor generation now explicitly requires source info.** The README
  uses `buf build --include-source-info -o descriptor.binpb` as the canonical
  downstream command and explains why stripping `SourceCodeInfo` breaks
  projected descriptions.

## v0.3.0 — 2026-07-03

### Changed

- **Python `connect_http` now uses per-connection transport policy.** The new
  shape is:
  `server.connect_http(base_url, auth=..., service_config=..., options=ChannelOptions(...), observer=...)`.
  One pooled `httpx.AsyncClient` is created per connection and shared by every
  registered HTTP-proxy handler from that call.
- **Retries now come from gRPC service config.** `retry_policy` supports
  `max_attempts`, `initial_backoff`, `max_backoff`, `backoff_multiplier`, and
  `retryable_status_codes` in gRPC code space. `method_config.name: [{}]` is
  the gRPC wildcard default. Retryable status codes may be names or enum
  numbers, including `UNKNOWN`, so unmapped HTTP statuses can be retried
  deliberately. HTTP responses are mapped to gRPC status codes before retry
  selection; HTTP 502 now maps to `UNAVAILABLE` instead of `UNKNOWN`. Invalid
  configs fail loudly at `connect_http()` with `ValueError`: duplicate
  `method_config.name` entries are rejected, `retry_unsafe_methods` must be
  boolean even when no retry policy is present, and `max_attempts` must be an
  integer capped at gRPC's effective maximum of 5. Backoff uses gRPC A6 full
  jitter. HTTP `Retry-After` is honored exactly when it is within `max_backoff`
  and resets the exponential backoff state; if the server pushback exceeds
  `max_backoff`, Invariant stops retrying and surfaces `RetryInfo` with the
  server's requested delay. The only transcoding-specific extension is
  `retry_unsafe_methods`, defaulting off.
- **Outbound HTTP errors now carry standard `google.rpc` details.** HTTP
  failures include `ErrorInfo`, `RetryInfo` when pushback is present, and
  `QuotaFailure` for rate-limit/quota exhaustion. Every remote detail dict is
  preserved in Connect/MCP/CLI JSON payloads; details that resolve as protobuf
  `Any` values also flow through gRPC rich status trailers. The `Retry-After`
  header drives retry scheduling, while a remote body `RetryInfo` wins in the
  surfaced error details.
- **`google.api.HttpBody` proxying is now production-strength.** HttpBody
  responses send `Accept: */*`, follow redirects, stream through
  `ChannelOptions.max_receive_message_size` while reading, preserve
  `content_type`, and participate in the same retry and observer path as JSON
  responses. Redirect following is fixed on for HttpBody responses, and
  response headers are surfaced to observers rather than copied onto the
  returned `HttpBody`.
- **Response observers now fire for success and error HTTP responses.**
  `OutboundHTTPResponse` now includes `headers`, `duration_ms`, and `success`.

### Removed

- **Python server-global outbound HTTP hooks were removed.** These methods no
  longer exist: `use_http_header_provider`, `use_http_query_provider`, and
  `use_http_response_observer`.

Migration mapping:

| v0.2.x | v0.3.0 |
|--------|--------|
| `server.use_http_header_provider(fn); server.connect_http(url)` | `server.connect_http(url, auth=fn)` or `auth=HTTPAuth(header_provider=fn)` |
| `server.use_http_query_provider(fn); server.connect_http(url)` | `server.connect_http(url, auth=HTTPAuth(query_provider=fn))` |
| `server.use_http_response_observer(fn); server.connect_http(url)` | `server.connect_http(url, observer=fn)` |
| `server.connect_http(url, timeout=30.0)` | `server.connect_http(url, options=ChannelOptions(connect_timeout=30.0, read_timeout=30.0))` |
| Built-in GET/HEAD retries on `429`/`5xx` | Add `service_config={"method_config": [{"name": [...], "retry_policy": {...}}]}` |
| Retrying POST/PUT/PATCH/DELETE impossible | Add `retry_unsafe_methods: True` on that method config |

### Added

- **Python `ChannelOptions`.** Supports `max_receive_message_size`,
  connect/read/write/pool timeout split, pool limits, keepalive expiry, proxy
  URL, and optional HTTP/2. `socks5://` proxy URLs require HTTPX's SOCKS extra
  in the application environment.
- **Python `HTTPAuth`.** Holds per-connection header and query providers; both
  run once per attempt so signatures and timestamps stay fresh after retries.
- **Go/Rust parity is a follow-up.** Go still has `UseHTTPHeaderProvider`, and
  Rust has no matching outbound HTTP transport-policy layer in this release.

## v0.2.8 — 2026-06-01

### Fixed

- **`connect_http` preserves a trailing slash in the path (Python).** A
  google.api.http path like `get: "/questions/"` now produces a request to
  `/questions/` instead of `/questions` — required by APIs (e.g. Django REST
  Framework) that 301-redirect or 404 without it. The root path `"/"` and
  slash-less paths are unaffected.

## v0.2.7 — 2026-06-01

### Added

- **`google.api.HttpBody` request/response support in `connect_http` (Python).**
  An RPC whose response type is `google.api.HttpBody` receives the raw, undecoded
  body bytes in `data` (and the MIME type in `content_type`) with no JSON↔proto
  mapping — the escape hatch for endpoints whose payload isn't worth modeling as
  a message. Symmetrically, an `HttpBody` request sends its `data` bytes verbatim
  with `content_type`. Other response types are unchanged.

## v0.2.6 — 2026-06-01

### Added

- **`Server.use_http_query_provider` for `connect_http` (Python).** Symmetric to
  `use_http_header_provider`, but injects query-string parameters into each
  outbound request — for APIs that authenticate via the query string (a plain
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
  raw response archive) independent of what the response message models. The
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
  to pin any wire key (e.g. snake_case for APIs that expect it).

## v0.2.3 — 2026-05-24

### Added

- **Per-method body caps via `Server.ConfigureMethod` (Go).** New
  `MethodConfig{MaxUnaryRequestBytes, MaxStreamRequestBytes}` type lets
  one outlier RPC (Upload, BulkImport) accept large bodies while the
  rest of the service stays tightly capped. Zero-valued fields inherit
  the server-level setting; non-zero override per method.

  Use case: `/files.v1.FileService/Upload` legitimately needs 1 GiB, but ListDir
  should reject anything over a few KiB. Before, you had to either
  raise the server-wide cap (and lose the safety on every other RPC)
  or write custom middleware. Now:
  `srv.ConfigureMethod("/files.v1.FileService/Upload", invariant.MethodConfig{MaxUnaryRequestBytes: 1 << 30})`
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
