# Agents Guide

Notes for AI agents working on this codebase.

## What this project does

One protobuf definition → all protocols. Write a `.proto` file with comments, and Invariant projects your services into:

- **MCP** (Model Context Protocol) — AI agents discover and call your RPCs as tools
- **CLI** — humans and shell-based agents call RPCs from the terminal
- **HTTP** — Connect endpoints over the canonical gRPC method paths
- **gRPC** — native generated-service registration and language-standard serving
- **Data schemas** — explicitly selected messages compile once into a versioned
  logical bundle rendered as Arrow, Parquet, Iceberg, or PostgreSQL

The core idea: proto comments become tool descriptions, field comments become JSON Schema descriptions, enums become constrained choices. Zero glue code.

## Architecture

```
.proto (authored source)
  └─ buf build → descriptor.binpb
       ├─ buf generate → standard protobuf/gRPC bindings → application service
       │                                                    │
       ├─ invariant.Server ←──── generated registration ────┘
       │    └─ native gRPC / HTTP / MCP / CLI
       └─ invariant-schema → SchemaBundle → Arrow / Parquet / Iceberg / PostgreSQL
```

The descriptor image is a compiled artifact, not a second authored contract.
Run normal code generation at build time from that exact image. Runtime
descriptor interpretation is for discovery, validation, reflection, dynamic
remote proxies, and optional projections; never generate application code at
runtime and never generate bespoke MCP/CLI/HTTP stubs.

Go, Python, Rust, and TypeScript implementations follow the same flow where
their runtime surface exists:

1. **Descriptor parsing** (`descriptor.go` / `descriptor.py`) — extract services, methods, messages, enums, and source comments from `FileDescriptorSet`
2. **Schema generation** (`schema.go` / `schema.py`) — convert proto message types to JSON Schema
3. **Service registration** — generated `Register<Service>Server` functions in
   Go, `add_<Service>Servicer_to_server` helpers in Python, generated Tonic
   registration helpers in Rust, and Protobuf-ES `DescService` registration in
   TypeScript are the local APIs. Idiomatic `connect_grpc` / `connectGrpc` and
   `connect_http` / `connectHttp` APIs register remote unary projections.
4. **Invoke dispatch** (`mcp.go:invoke` / `server.py:_invoke`) — proto-in/proto-out core with interceptor chain
5. **Projections** — boundary converters that translate each protocol's wire format to/from proto messages

### Shape mirror, not literal mirror

Go, Python, Rust, and TypeScript share the same dispatch pipeline, but the
implementations are **idiomatic per language**. Python and TypeScript are async
end-to-end; Go is sync (goroutines + sync function signatures). Don't try to
keep them literally identical when the language idiom differs. Prefer
language-native patterns over forced symmetry.

`conformance/feature-parity.json` is the release contract. Every portable Core
feature needs idiomatic APIs and behavioral test evidence in Go, Python, Rust,
and TypeScript before a repository tag may ship. Run `make parity` while
developing and `make parity-release` before tagging. Do not mark a row supported
merely because similarly named methods exist; metadata, statuses, deadlines,
cancellation, limits, lifecycle, locally generated message types, and method
identities are part of the behavior. Descriptor-only remote adapters may use
dynamic protobuf messages at their boundary.

Build-tool and ecosystem capabilities are different. The descriptor-to-
SchemaBundle compiler is implemented once and is available to every language;
duplicating it would create competing canonical mappings. A bridge such as
protobuf messages to `pyarrow.Table` is explicitly a Python ecosystem adapter,
not a missing Rust or TypeScript server feature.

### TypeScript

The TypeScript package in `typescript/` is a descriptor-driven Node runtime.
Its HTTP/RPC projection uses Connect-ES with generated Protobuf-ES descriptors.
`Server.register(service, implementation)` accepts the generated `DescService`
and its typed `ServiceImpl<T>`. Shared middleware uses Connect-ES `Interceptor`;
native gRPC uses grpc-js. Do not add parallel custom handler or interceptor
types.

### Async-native Python (load-bearing)

Python is async-only. Generated service registration rejects sync handlers via
`inspect.iscoroutinefunction`. Interceptors must be standard async
`grpc.aio.ServerInterceptor` instances. All projections (HTTP/MCP/gRPC/CLI)
and remote clients (`connect_grpc`, `connect_http`) are async.
There is no sync-compat layer and no detect-and-await.

- HTTP projection is an ASGI app served by uvicorn. Users mount it on their own ASGI app via `Server.asgi_app()`.
- gRPC projection uses `grpc.aio.server`.
- MCP reads stdin via `asyncio.StreamReader`.
- HTTP client (`HTTPDynamicHandler`) uses `httpx.AsyncClient`.
- `Server.serve_projections(...)` runs optional HTTP/MCP/CLI tasks with a
  cancellation cascade. Native gRPC is built once with `grpc_server()`; callers
  add ports/credentials and start the returned `grpc.aio.Server`.
- Generated local handlers receive a `grpc.aio.ServicerContext`-compatible
  context on every projection. HTTP invocation metadata is an explicit
  tracing/request-ID allowlist; never pass arbitrary authorization, tenant,
  principal, or role headers through. Stdio MCP and CLI contexts carry no
  caller metadata.
- Build one isolated `DescriptorPool` from the supplied FileDescriptorSet for
  dynamic remote proxies. Do not reintroduce `descriptor_pool.Default()` or
  generated-module import-order coupling on those paths.

### Programmatic invocation

Every runtime exposes idiomatic `invoke` and `invoke_stream` forms for dispatch
without binding a projection. These are useful for in-process callers and
tests, but they still freeze registration and use the shared dispatch chain.

### Graceful shutdown

Go's native lifecycle is `Server.Serve(net.Listener)` plus `GracefulStop()` or
`Stop()`. Python returns one caller-bound `grpc.aio.Server`; TypeScript returns
one caller-bound grpc-js `Server`; Rust exposes registered Tonic routes for the
caller's `tonic::transport::Server`. Optional projection runners must not own a
second native gRPC listener.

### Canonical data schemas

`proto/invariant/data/v1/schema.proto` is the language-neutral IR. There is one
descriptor compiler in `go/data`; Python, Rust, and TypeScript decode the same
generated bundle rather than reimplementing protobuf inference. Target
renderers live in isolated `go/data/{arrow,parquet,iceberg,postgres}` packages,
and `go/cmd/invariant-schema` is the build-time CLI.

Python's optional data surface maps a bundle dataset to `pyarrow.Schema` and
matching generated protobuf messages to `pyarrow.Table`; standard PyArrow owns
Parquet file writing. The SchemaBundle remains the mapping and evolution
authority. Do not infer a second schema independently from Python descriptors
or arbitrary dictionaries.

Protobuf is the only authored logical contract, not a physical storage format.
Compilation requires explicit fully-qualified root messages. The existing
output bundle is derived evolution state: numeric protobuf paths retain active
IDs, every removed nested/list/map ID is tombstoned, and reuse or a type/
presence change on an active ID fails. Never delete or hand-edit that history
to make a schema change pass; use a new protobuf field number and reserve the
old number/name.

Renderers emit a diagnostic per logical node. Do not silently collapse maps,
unsigned values, enum numbers, temporal precision/range, or recursive types.
Parquet schemas must be produced and tested through the official Arrow bridge,
with `PARQUET:field_id` metadata for real and synthetic fields. Iceberg IDs are
globally unique across struct fields, list elements, and map key/value fields.
Iceberg schemas target format v3: implicit scalar/enum and repeated/map fields
carry protobuf-compatible initial/write defaults, while protobuf `required`
fields are rejected because historical rows have no safe missing value.
PostgreSQL emits desired-state DDL directly for Atlas; do not introduce HCL as
an intermediate source.

The committed SchemaBundle—not an Arrow IPC projection—is the evolution
registry. Arrow-Go v18 preserves the map shape but does not round-trip custom
metadata attached specifically to map key/value children through IPC. Direct
bundle-to-Parquet rendering retains those child IDs and is covered end to end;
do not chain Parquet generation through the emitted Arrow IPC artifact.

Custom public protobuf options are deferred until Invariant has a globally
registered Protobuf extension number. Do not reuse downstream ad-hoc option
numbers. Dataset roots become append-only after the first bundle is committed;
reachable extension ranges and normalized storage-name collisions must fail
compilation. Keys, indexes, partitions, table placement, migrations, file writes,
and catalog commits remain downstream policy.

Go, Python, Rust, and TypeScript bundle readers reject unknown IR or mapping
versions by default. Raw generated messages remain wire types, not permission
to interpret a newer mapping with older code.

## Convention over configuration

- We do NOT support extensive configurability. Support common use cases well.
- Don't add feature flags, options structs, or builder patterns for hypothetical needs.
- If something works for 95% of cases, ship it. Don't add a knob for the other 5%.
- **Cut before you add.** When a feature path doesn't pull its weight, drop it. The framework should always be getting smaller relative to its capability surface.

## Stack stance

- **gRPC-driven, protobuf-driven.** This is the design center. Connect-Web for browser clients, gRPC for service-to-service, MCP for AI agents, CLI for shell. There is no first-class REST surface — REST routes are only consumed (via `connect_http` proxying) and never served.
- **Modern-forward.** Pick one format and update tests with it: Connect-style
  errors are lowercase and unwrapped, protobuf HTTP bodies use
  `application/proto`, and Python is async-only.

## Code style

- **No micro-optimizations.** Generated registration and proxy setup cache typed
  request/response factories. A binary marshal/unmarshal conversion is justified
  only where concrete generated and dynamic message types must cross; don't add
  similar optimizations without a measured need.
- **No unnecessary abstractions.** Three similar lines of code is better than a premature helper function.
- **Tests should test behavior, not properties.** Consolidate granular property-check tests into comprehensive behavior tests. A single `TestParseServices` that checks name, comment, methods, and types is better than 5 separate tests each checking one field.
- **Error handling should be practical.** Don't add validation for things that can't happen. Trust internal code paths.

## Things to know

### Go native and projected serving
`Server.Serve(listener)` owns the canonical native gRPC lifecycle. Pass ordinary
`grpc.ServerOption` values to `ServerFromDescriptor` or `ServerFromBytes`.
`ServeProjections(ctx, ...)`
runs optional projections in parallel; the first completion cancels the others
and waits for their shutdown.

Generated service registration and configuration freeze when native serving or
projection execution begins. Because `grpc.ServiceRegistrar` cannot return an
error, a late generated registration panics deterministically. Register services,
filters, shared interceptors, HTTP limits, and metadata mappers before serving.

Use generated `Register<Service>Server` functions, `Serve(listener)`,
`ConnectGRPC`, and grpc-go's `grpc.UnaryServerInterceptor` and
`grpc.StreamServerInterceptor` types directly. `HTTPHandler()` returns one
`http.Handler`. Do not reintroduce reflection-based Go registration, port-owning
gRPC projections, alternate lifecycle spellings, or Invariant-specific stream
and interceptor aliases.

Constructor `grpc.ServerOption` interceptors apply only to native gRPC. Explicit
`Use` / `UseStream` grpc-go interceptors apply once to every projection, including
native gRPC; registering the same function in both places intentionally runs it
twice.

### MCP protocol compliance
MCP uses JSON-RPC 2.0 and the stable `2025-11-25` protocol version. Key rules:
- Requests without `id` are notifications — no response
- Parse errors return a response with `null` id and error code `-32700`
- Method not found returns error code `-32601`
- Initialize requests require string `protocolVersion`, object `capabilities`,
  and `clientInfo` with string `name` and `version`; unsupported requested
  versions negotiate to the sole supported `2025-11-25` version.
- Portable numeric request IDs are integers in the JavaScript-safe range
  `-(2^53-1)` through `2^53-1`; use strings for larger identifiers.
- Stdio sessions track in-flight calls for `notifications/cancelled`.
- `POST /mcp` is stateless: cancellation and deadlines are scoped to that
  request, and a separate cancellation POST cannot target an earlier POST.

### Include/Exclude filtering
All four runtimes support idiomatic include/exclude globs for optional
projection catalogs: `Include` / `Exclude` in Go, `include` / `exclude` in
Python and Rust, and `include` / `exclude` in TypeScript. Go, Python, and
TypeScript accept multiple patterns; Rust accepts one pattern per call and
returns a `Result` because configuration may already be frozen.
- Environment variables: `INVARIANT_INCLUDE`, `INVARIANT_EXCLUDE` (comma-separated)
- `*` matches any characters including dots
- Exclude is applied after include
- Configure filters before generated or proxy registration; they
  determine which methods enter the projection catalog at registration time.
- The complete generated native service remains registered. Filters never make
  native methods uncallable.

### HTTP is Connect-only
The HTTP projection serves only the canonical Connect route:
`POST /{package.Service}/{Method}`. Unary bodies use `application/json` or
`application/proto`; streaming uses the Connect streaming content types. There
is no server-side `google.api.http` REST routing. Go, Python, and TypeScript
remote HTTP clients may consume the primary annotation; the portable remote
HTTP contract is the canonical Connect method path.

HTTP request headers are untrusted. The default mapper forwards only tracing and
correlation values; a custom `HTTPMetadataMapper` still cannot assert authorization,
tenant, principal, role, user, protocol, or `invariant-internal-*` metadata.
Authenticate in enclosing HTTP middleware. Do not turn a caller-controlled
header into trusted identity metadata.

### HTTP error format
Connect-style envelope only: `{"code": "invalid_argument", "message": "...", "details": [...]}`. Lowercase code, no wrapper, no toggle. The `connect_http` client parses the same canonical format.

### Tool catalog and descriptor endpoints
- `GET /` and `GET /__invariant/tools` → `{"tools": [...]}` (same shape as MCP `tools/list`).
- `GET /__invariant/descriptor.binpb` → raw FileDescriptorSet bytes for tooling.

### Health probes
- `GET /healthz` and `GET /readyz` → `{"status":"ok"}`. Always 200 once the
  HTTP handler is built — registration is synchronous, so by the time we
  answer requests we are ready. Don't gate on app-level health (no liveness
  signal hooks); users wanting that can register their own service.
- No gRPC `grpc.health.v1.Health` service is synthesized. Register the
  ecosystem's generated health service when needed; include it in the runtime
  descriptor image in runtimes that require descriptor agreement. Go's
  `grpc.ServiceRegistrar` also accepts infrastructure services absent from the
  projected descriptor.

### Panic / exception recovery
- Go: the shared unary and stream interceptor terminals install a `defer recover()` that
  converts panics into `codes.Internal` status errors so a single goroutine
  bug can't crash the server. The wrapped error names the method path for
  triage.
- Python: every projection wraps dispatch exceptions and normalizes them
  through `as_invariant_error`. HTTP/Connect emits the canonical Connect error
  envelope, native gRPC emits a gRPC status, and MCP and CLI map the normalized
  error at their own transport boundaries. Mid-stream HTTP raises land in the
  Connect end-stream envelope with the original code preserved.
  `asyncio.CancelledError` intentionally **propagates** from `mcp_call_tool`
  instead of being swallowed into a "cancelled" response — that lets the stdio
  task scheduler clean up cancelled requests without a response (MCP spec) and
  lets `asyncio.timeout` in the HTTP path convert cancellation to
  `deadline_exceeded`.
- Rust: the typed shared terminal wraps the entire chain in
  `AssertUnwindSafe(...).catch_unwind()` — panics become `Code::Internal`
  status errors with the panic message and method path.

### Rust native gRPC
Generated Tonic clients, traits, codecs, requests, responses, and status
trailers are the native surface. `Server::grpc_routes()` freezes and extracts
the registered routes, including reflection from the exact descriptor image,
for a caller-owned `tonic::transport::Server`. Do not restore a hand-written
gRPC frame parser or a port-owning native projection.

### Resource limits
- Every runtime gives HTTP unary request, encoded unary response, streaming
  request message, and encoded streaming response message independent 16 MiB
  defaults. Exceeded → `resource_exhausted`. The idiomatic per-method config
  overrides each limit for one full gRPC method.
- Connect streaming request framing is inspected before payload allocation, so
  a forged size won't allocate a giant buffer. Streaming response limits apply
  per message, not to the lifetime of a stream.
- Native receive/send limits remain ordinary grpc-go, grpcio, Tonic, or
  grpc-js controls. They do not govern standalone HTTP JSON bytes.
- `Connect-Timeout-Ms` is honored on every HTTP path: unary, streaming, and
  the `/mcp` JSON-RPC transport. The header is a positive integer of at most
  ten ASCII digits; malformed values return `invalid_argument`. The one
  absolute deadline covers body reading, decoding, and application work. On
  streaming, a deadline that expires after the response starts is delivered in
  the end-stream envelope rather than changing the HTTP status.

### gRPC reflection
Go, Python, and TypeScript register reflection on their one native server.
Rust's `grpc_routes()` includes the conventional Tonic reflection service.
Keep reflection enabled in documented native deployments.

### Validation
Go `Validation` / `ValidationStream`, Python `validation`, and TypeScript
`validation` are opt-in maintained Protovalidate adapters. Python uses one
standard `grpc.aio.ServerInterceptor`; TypeScript uses one Connect-ES
`Interceptor`; both cover unary and streaming. Failures short-circuit with
`invalid_argument` plus field-level `BadRequest` details. Rust remains
explicitly unavailable until there is a maintained ecosystem adapter.

### Performance targets
Use `go/benchmarks_test.go` and `python/bench/bench.py` as the current reference.
Go generated registration precomputes typed request factories, so the HTTP
request path must not rebuild descriptor registries or discover handler types
per call. Never call `protodesc.NewFiles` on a request path.

### Proto descriptor requirement
`buf build -o descriptor.binpb` — Buf 1.71 includes source info by default. Do
not pass `--exclude-source-info`, or comments won't be available for tool
descriptions. Then run `buf generate descriptor.binpb` so generated bindings
and runtime projections are derived from the exact same compiled graph.

## Running

```bash
flox activate
make generate        # regenerate proto stubs and descriptor artifacts
make check           # deterministic quality checks and all four test suites
make security        # secrets, integrity, and vulnerability checks
make integration     # clean Git installs for all four languages
make parity-release  # strict portable-feature gate before one root tag
```

## Dependency boundaries

The root `VERSION` is the release version for every language package. CI checks
that package metadata and runtime version constants stay synchronized.
Releases use one repository tag, `vX.Y.Z`; do not create new language-prefixed
tags.

Invariant-owned packages are distributed only from Git. Do not publish them to,
or document installation from, PyPI, the npm registry, crates.io, or another
language registry. Release documentation may use the shared root tag;
production installations may use each tool's full-commit revision syntax. Keep
registry guards enabled where the ecosystem supports them (`private: true` for
npm and `publish = false` for Cargo, plus the `Private :: Do Not Upload`
Python classifier). Repository policy and the absence of publication
automation remain additional guardrails. Keep the clean Git-install check for
all four language packages in CI.

Dependency roots and lockfiles:

- **`.flox/env/manifest.toml`** — language toolchains and CLI tools (`python3`,
  `uv`, `go`, `buf`, `golangci-lint`, `ruff`, `protoc`, `protoc-gen-go`,
  `protoc-gen-go-grpc`). Flox
  may provide a bootstrap Go command while `GOTOOLCHAIN` selects the exact
  checksum-verified patch release required by `go.mod` when the Flox catalog
  lags a security release.
- **`python/pyproject.toml` + `python/uv.lock`** — every Python runtime and dev
  dep. `uv run` resolves against this. PyArrow belongs in the optional `data`
  extra and the dev test group; importing the core RPC package must not import
  PyArrow.
- **`go.mod` + `go.sum`** — every Go dep. The root module keeps Go packages in
  `go/` while allowing one repository-wide `vX.Y.Z` release tag. Consumers run
  `go get github.com/jim-technologies/invariantprotocol/go@vX.Y.Z` and import
  that package path; their `go.mod` records the root module
  `github.com/jim-technologies/invariantprotocol`. Never recreate a nested Go
  module or new `go/vX.Y.Z` tags. Apache Arrow/Parquet and Iceberg dependencies
  belong only to the isolated data-renderer packages; do not pull them into the
  RPC runtime package.
- **`rust/Cargo.toml` + `rust/Cargo.lock`** — the Rust runtime and codegen
  workspace. Commit the lockfile and use `--locked` in integrity checks.
- **`package.json` + `package-lock.json`** — every TypeScript runtime and dev
  dep. TypeScript source remains in `typescript/`; `npm ci` resolves from the
  repository root.

CI (`.github/workflows/ci.yml`) runs everything inside `flox activate`, so contributors and CI hit the same toolchain by construction.

## Streaming

Unary and server-streaming RPCs are projected across all four surfaces.
Client-streaming and bidi methods remain fully available on native generated
gRPC services, but are intentionally omitted from HTTP, MCP, CLI, and remote
proxy projection catalogs.

- **Handler shape (Go)**: implement the generated server interface, including
  `func(*Req, grpc.ServerStreamingServer[Resp]) error`, and register it with
  the generated `Register<Service>Server` function.
- **Handler shape (Python)**: `async def Method(self, request, context)` declared
  as an async generator (`yield response`). Implement the complete generated
  servicer and register it with `add_<Service>Servicer_to_server`. Registration
  rejects coroutines with a clear error so the mismatch is caught at startup.
- **Wire formats**:
  - gRPC: native server-streaming (`grpc.StreamDesc`).
  - HTTP: Connect streaming envelopes (`application/connect+json` for
    text, `application/connect+proto` for binary). Plain `application/json`
    on a streaming endpoint is rejected — Connect splits unary and streaming
    content types intentionally. End-stream envelope is always JSON (Connect
    spec) regardless of the message content type.
  - MCP: each emitted message becomes a text block in the `content` array.
    Errors mid-stream surface as `isError` with the chunks already emitted
    plus a trailing error text block.
  - CLI: chunks use newline-delimited JSON. Host-facing writers may flush each
    chunk; string-returning test helpers buffer by contract.
- **Shared middleware**: Go uses the separate standard `Use` / `UseStream`
  grpc-go types; Python uses one standard `grpc.aio.ServerInterceptor` through
  `use`; Rust uses `use_shared_unary` / `use_shared_stream`; TypeScript uses one
  Connect-ES `Interceptor` through `use`. First registered is outermost.
- **Proxying**: remote gRPC and HTTP registration skips streaming methods.

## MCP Streamable HTTP transport

The HTTP projection also serves the non-SSE MCP `2025-11-25` tool-server subset
at `POST /mcp`. Clients must advertise both JSON and SSE in `Accept`, although
Invariant responds with JSON. Initialization may omit `MCP-Protocol-Version`;
subsequent requests must send the exact current version. Notifications and
client responses return `202 Accepted` with no body, `GET /mcp` returns `405`,
and an `Origin` header is rejected with `403`. Reuse the same dispatch helper
as stdio; do not add a second tool registry.

## Not yet implemented

- Full `connect_http` client path-template grammar beyond `{field}`, `{field=*}`, `{field=**}`
- `connect_http` client selection among `additional_bindings`
- Client-streaming and bidi projections (native generated gRPC supports them)
- Rust `google.api.http` transcoding for the remote HTTP client; its portable
  path currently uses canonical Connect routes
- Go-native protobuf-record to Arrow conversion and framework-owned data-file writing
- Iceberg catalog commits, partition policies, and table migration/application
- Relational keys, indexes, normalization, and SQL dialects beyond PostgreSQL
- Public dataset/column protobuf annotations (waiting on a globally registered
  extension number)
