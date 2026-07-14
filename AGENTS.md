# Agents Guide

Notes for AI agents working on this codebase.

## What this project does

One protobuf definition → all protocols. Write a `.proto` file with comments, and Invariant projects your services into:

- **MCP** (Model Context Protocol) — AI agents discover and call your RPCs as tools
- **CLI** — humans and shell-based agents call RPCs from the terminal
- **HTTP** — Connect endpoints over the canonical gRPC method paths
- **gRPC** — native generated-service registration and normal grpc-go serving

The core idea: proto comments become tool descriptions, field comments become JSON Schema descriptions, enums become constrained choices. Zero glue code.

## Architecture

```
.proto + generated gRPC service → invariant.Server → native gRPC / HTTP / MCP / CLI
             descriptor.binpb ────────────────┘
```

Go, Python, Rust, and TypeScript implementations follow the same flow where
their runtime surface exists:

1. **Descriptor parsing** (`descriptor.go` / `descriptor.py`) — extract services, methods, messages, enums, and source comments from `FileDescriptorSet`
2. **Schema generation** (`schema.go` / `schema.py`) — convert proto message types to JSON Schema
3. **Service registration** — generated `Register<Service>Server` functions are
   primary in Go; reflection-based `Register()` is compatibility-only.
   `ConnectGRPC()` and `ConnectHTTP()` register remote unary projections.
4. **Invoke dispatch** (`mcp.go:invoke` / `server.py:_invoke`) — proto-in/proto-out core with interceptor chain
5. **Projections** — boundary converters that translate each protocol's wire format to/from proto messages

### Shape mirror, not literal mirror

Go, Python, Rust, and TypeScript share the same dispatch pipeline, but the
implementations are **idiomatic per language**. Python and TypeScript are async
end-to-end; Go is sync (goroutines + sync function signatures). Don't try to
keep them literally identical when the language idiom differs. Prefer
language-native patterns over forced symmetry.

### TypeScript

The TypeScript package in `typescript/` is a descriptor-driven Node runtime. Its
HTTP/RPC projection uses Connect-ES (`@connectrpc/connect` +
`@connectrpc/connect-node`) with runtime Protobuf-ES descriptors, not generated
server stubs. It supports local servicer registration, remote gRPC proxying via
`connect()`, remote HTTP proxying via `connectHttp()`, unary and
server-streaming `invoke`, unary and stream interceptors, JSON Schema/tool
catalogs, CLI helpers, MCP dispatch including `POST /mcp`, Node HTTP/Connect
JSON/proto + streaming envelopes, and grpc-js serving with reflection. Python
still has the richest HTTP retry and `google.api.HttpBody` handling.

### Async-native Python (load-bearing)

Python is async-only. `register()` rejects sync handlers via `inspect.iscoroutinefunction`. Interceptors must be async. All projections (HTTP/MCP/gRPC/CLI) and remote clients (`connect`, `connect_http`) are async. There is no sync-compat layer and no detect-and-await.

- HTTP projection is an ASGI app served by uvicorn. Users mount it on their own ASGI app via `Server.asgi_app()`.
- gRPC projection uses `grpc.aio.server`.
- MCP reads stdin via `asyncio.StreamReader`.
- HTTP client (`HTTPDynamicHandler`) uses `httpx.AsyncClient`.
- `Server.serve(...)` is `async def`. Multi-projection uses `asyncio.gather` with cancel cascade.

### Programmatic invocation

`Server.Invoke(ctx, toolName, request)` (Go) and `await Server.invoke(tool_name, request)` (Python) dispatch a tool by name without binding a projection — useful for in-process callers (workflow runtimes, tests).

### Graceful shutdown

Go's native lifecycle is `Server.Serve(net.Listener)` plus `GracefulStop()` or
`Stop()`. `Server.ServeProjections(ctx, ...)` runs optional HTTP/MCP/CLI
projections and honors cancellation. Python's `await server.serve(...)`
propagates `asyncio.CancelledError` to all projection tasks.

## Convention over configuration

- We do NOT support extensive configurability. Support common use cases well.
- Don't add feature flags, options structs, or builder patterns for hypothetical needs.
- If something works for 95% of cases, ship it. Don't add a knob for the other 5%.
- **Cut before you add.** When a feature path doesn't pull its weight, drop it. The framework should always be getting smaller relative to its capability surface.

## Stack stance

- **gRPC-driven, protobuf-driven.** This is the design center. Connect-Web for browser clients, gRPC for service-to-service, MCP for AI agents, CLI for shell. There is no first-class REST surface — REST routes are only consumed (via `connect_http` proxying) and never served.
- **No legacy compat.** Modern-forward. We pick one format and update tests instead of preserving the old one. Examples: Connect-style errors only (lowercase, unwrapped), `application/proto` only (no `application/x-protobuf`), Python is async-only.

## Code style

- **No micro-optimizations.** Generated registration and proxy setup cache typed
  request/response factories. A binary marshal/unmarshal conversion is justified
  only on compatibility paths where concrete generated and dynamic message types
  must cross; don't add similar optimizations without a measured need.
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

Constructor `grpc.ServerOption` interceptors apply only to native gRPC. Explicit
`Use` / `UseStream` grpc-go interceptors apply once to every projection, including
native gRPC; registering the same function in both places intentionally runs it
twice.

### MCP protocol compliance
MCP uses JSON-RPC 2.0 over stdio. Key rules:
- Requests without `id` are notifications — no response
- Parse errors return a response with `null` id and error code `-32700`
- Method not found returns error code `-32601`

### Include/Exclude filtering
Both Go and Python support glob-based filtering of which methods enter optional
projection catalogs:
- `server.Include("*.Greet")` / `server.include("*.Greet")`
- `server.Exclude("*Poll*")` / `server.exclude("*Poll*")`
- Environment variables: `INVARIANT_INCLUDE`, `INVARIANT_EXCLUDE` (comma-separated)
- `*` matches any characters including dots
- Exclude is applied after include
- Configure filters before generated, reflection, or proxy registration; they
  determine which methods enter the projection catalog at registration time.
- Go native gRPC always retains the complete generated `ServiceDesc`. Filters
  do not mutate the canonical gRPC service or make its methods uncallable.

### HTTP is Connect-only
The HTTP projection serves only the canonical Connect route: `POST /{package.Service}/{Method}`. Unary bodies use `application/json` or `application/proto`; streaming uses the Connect streaming content types. There is no server-side `google.api.http` REST routing — those annotations are still read by the `connect_http` *client* for proxying to legacy REST APIs we don't own.

HTTP request headers are untrusted. The default mapper forwards only tracing and
correlation values; a custom `HTTPMetadataMapper` still cannot assert authorization,
tenant, principal, role, user, protocol, or `invariant-internal-*` metadata.
Authenticate in HTTP middleware and inject trusted incoming gRPC metadata into
the request context.

### HTTP error format
Connect-style envelope only: `{"code": "invalid_argument", "message": "...", "details": [...]}`. Lowercase code, no wrapper, no toggle. The `connect_http` client is tolerant — accepts both this format and the legacy wrapped `{"error": {...}}` format from remote services.

### Tool catalog and descriptor endpoints
- `GET /` and `GET /__invariant/tools` → `{"tools": [...]}` (same shape as MCP `tools/list`).
- `GET /__invariant/descriptor.binpb` → raw FileDescriptorSet bytes for tooling.

### Health probes
- `GET /healthz` and `GET /readyz` → `{"status":"ok"}`. Always 200 once the
  HTTP handler is built — registration is synchronous, so by the time we
  answer requests we are ready. Don't gate on app-level health (no liveness
  signal hooks); users wanting that can register their own service.
- No gRPC `grpc.health.v1.Health` service is auto-registered. In Go, register
  grpc-go's normal health server with `grpc_health_v1.RegisterHealthServer`;
  `invariant.Server` implements `grpc.ServiceRegistrar`. Other languages can
  register a health service normally when needed.

### Panic / exception recovery
- Go: the shared unary and stream interceptor terminals install a `defer recover()` that
  converts panics into `codes.Internal` status errors so a single goroutine
  bug can't crash the server. The wrapped error names the method path for
  triage.
- Python: every projection wraps the dispatch in `try/except Exception` and
  routes through `as_invariant_error`, so the wire response is always a
  well-formed Connect error envelope. Mid-stream raises land in the Connect
  end-stream envelope with the original code preserved. `asyncio.CancelledError`
  intentionally **propagates** from `mcp_call_tool` instead of being swallowed
  into a "cancelled" response — that lets the stdio task scheduler clean up
  cancelled requests without a response (MCP spec) and lets `asyncio.timeout`
  in the HTTP path convert cancellation to `deadline_exceeded`.
- Rust: `chained_invoke` and `chained_invoke_stream` wrap the entire chain
  in `AssertUnwindSafe(...).catch_unwind()` — panics become `Code::Internal`
  status errors with the panic message + method path. Mirrors Go's behaviour.

### Rust gRPC trailers
- `grpc::grpc_stream_response` returns `Body::new(StreamBody::new(stream))`
  yielding `Frame::data(...)` per emitted message and a final `Frame::trailers(...)`
  carrying `grpc-status` + `grpc-message`. That's the gRPC-spec-correct shape
  (status in trailers, not headers). End-to-end test asserts a tonic client
  surfaces the `FailedPrecondition` code from a mid-stream error.

### Multi-projection serve (Rust)
- `projections::serve::serve(server, projections, cancel)` runs an iterable
  of `Projection::{Http, Grpc, McpStdio}` in parallel. The first projection
  to complete (or the cancellation token firing) signals the rest to shut
  down. Mirrors Go's `Server.ServeProjections(ctx, projections...)` and Python's
  `await server.serve(http=..., grpc=..., mcp=True)`.

### Resource limits
- Go HTTP unary request and encoded response bodies each default to independent
  16 MiB caps. Connect stream request and encoded response messages also have
  independent 16 MiB per-message caps. Exceeded → `resource_exhausted`.
  `ConfigureMethod` can override each limit for one full gRPC method.
- Connect streaming request framing is inspected before payload allocation, so
  a forged size won't allocate a giant buffer. Streaming response limits apply
  per message, not to the lifetime of a stream.
- Native `grpc.MaxRecvMsgSize` and `grpc.MaxSendMsgSize` remain normal gRPC
  protobuf-message limits; they do not govern standalone HTTP JSON bytes.
- `Connect-Timeout-Ms` is honored on every HTTP path: unary, streaming, and
  the `/mcp` JSON-RPC transport. On streaming, the deadline-exceeded error
  is delivered in the end-stream envelope rather than HTTP status — that's
  the Connect-correct shape (the response has already been started by the
  time the deadline fires).

### gRPC reflection
Always registered. `grpcurl`, Buf Studio, and Connect debug clients work without extra setup. Don't gate this — it's table stakes for gRPC-driven workflows.

### Validation
`invariant.Validation()` (Go) / `invariant.validation()` (Python) — opt-in interceptor running `protovalidate`. Failures short-circuit with `invalid_argument` plus field-level `BadRequest` details.

### Performance targets
Use `go/benchmarks_test.go` and `python/bench/bench.py` as the current reference.
Go generated registration precomputes typed request factories, so the HTTP
request path must not rebuild descriptor registries or discover handler types
per call. Never call `protodesc.NewFiles` on a request path.

### Proto descriptor requirement
`buf build -o descriptor.binpb` — Buf 1.71 includes source info by default. Do
not pass `--exclude-source-info`, or comments won't be available for tool
descriptions.

## Running

```bash
flox activate
make test      # run all tests (Go + Python + Rust + TypeScript)
make lint      # lint all code
make fmt       # auto-format
make generate  # regenerate proto stubs
```

## Dependency boundaries

Four lockfiles:

- **`.flox/env/manifest.toml`** — language toolchains and CLI tools (`python3`, `uv`, `go`, `buf`, `golangci-lint`, `ruff`, `protoc`, `protoc-gen-go`). May also pin nix-built Python packages when uv would otherwise need a C toolchain inside the flox sandbox.
- **`python/pyproject.toml` + `python/uv.lock`** — every Python runtime and dev dep. `uv run` resolves against this.
- **`go/go.mod` + `go/go.sum`** — every Go dep.
- **`typescript/package.json` + `typescript/package-lock.json`** — every TypeScript runtime and dev dep. `npm ci` resolves against this.

CI (`.github/workflows/ci.yml`) runs everything inside `flox activate`, so contributors and CI hit the same toolchain by construction.

## Streaming

Unary and server-streaming RPCs are projected across all four surfaces.
Client-streaming and bidi methods remain fully available on native generated
gRPC services, but are intentionally omitted from HTTP, MCP, CLI, and remote
proxy projection catalogs.

- **Handler shape (Go)**: implement the generated server interface, including
  `func(*Req, grpc.ServerStreamingServer[Resp]) error`, and register it with
  the generated `Register<Service>Server` function. The old
  `func(*Req, invariant.ServerStream) error` reflection shape remains only as
  a compatibility convenience.
- **Handler shape (Python)**: `async def Method(self, request, context)` declared
  as an async generator (`yield response`). Registration rejects coroutines
  with a clear error so the mismatch is caught at startup.
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
  - CLI: chunks are buffered and printed as newline-delimited JSON (one chunk
    per line). Mirrors Go's CLI behaviour — no real-time output here, since
    the `run_cli` contract returns a single string.
- **Stream interceptors**: `UseStream` / `use_stream`. Separate from `Use` /
  `use` because the wire signatures genuinely differ (returns a stream, not a
  single value). Same registration order semantics — first registered =
  outermost.
- **Proxying**: `ConnectGRPC` (gRPC proxy; `Connect` is a compatibility spelling)
  and `ConnectHTTP` (REST proxy) skip
  streaming methods. Forwarding a stream through a proxy duplicates what gRPC
  already does, without adding value here.

## MCP Streamable HTTP transport

The HTTP projection also serves MCP at `POST /mcp` — one JSON-RPC request per
POST, one JSON-RPC response back, `204 No Content` on notifications. Reuses
the same `mcpDispatch` / `mcp_dispatch` helper as the stdio session so there
is one source of truth for what each MCP method does. SSE-based streaming
notifications are not implemented; users wanting `notifications/progress`
during a long tool call should call the gRPC or HTTP-Connect projection
directly.

## Not yet implemented

- Full `connect_http` client path-template grammar beyond `{field}`, `{field=*}`, `{field=**}`
- `connect_http` client selection among `additional_bindings`
- Client-streaming and bidi projections (native generated gRPC supports them)
