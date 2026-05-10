# Agents Guide

Notes for AI agents working on this codebase.

## What this project does

One protobuf definition → all protocols. Write a `.proto` file with comments, and Invariant projects your services into:

- **MCP** (Model Context Protocol) — AI agents discover and call your RPCs as tools
- **CLI** — humans and shell-based agents call RPCs from the terminal
- **HTTP** — REST-style endpoints with `google.api.http` transcoding support
- **gRPC** — standard gRPC server with dynamic dispatch (no generated server stubs)

The core idea: proto comments become tool descriptions, field comments become JSON Schema descriptions, enums become constrained choices. Zero glue code.

## Architecture

```
.proto → buf build → descriptor.binpb → Invariant runtime → MCP / CLI / HTTP / gRPC
```

Both Go and Python implementations follow the same flow:

1. **Descriptor parsing** (`descriptor.go` / `descriptor.py`) — extract services, methods, messages, enums, and source comments from `FileDescriptorSet`
2. **Schema generation** (`schema.go` / `schema.py`) — convert proto message types to JSON Schema
3. **Tool registration** — `Register()` (local servicer), `Connect()` (gRPC proxy), `ConnectHTTP()` (HTTP proxy)
4. **Invoke dispatch** (`mcp.go:invoke` / `server.py:_invoke`) — proto-in/proto-out core with interceptor chain
5. **Projections** — boundary converters that translate each protocol's wire format to/from proto messages

### Shape mirror, not literal mirror

Go and Python share the same 8-method API and the same dispatch pipeline, but the implementations are **idiomatic per language**. Python is async end-to-end; Go is sync (goroutines + sync function signatures). Don't try to keep them literally identical when the language idiom differs. Prefer language-native patterns over forced symmetry.

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

Go's `Server.Serve(ctx, projections...)` honors the context: cancellation triggers `http.Server.Shutdown` and `grpc.Server.GracefulStop` on every running projection. Python's `await server.serve(...)` propagates `asyncio.CancelledError` to all projection tasks.

## Convention over configuration

- We do NOT support extensive configurability. Support common use cases well.
- Don't add feature flags, options structs, or builder patterns for hypothetical needs.
- If something works for 95% of cases, ship it. Don't add a knob for the other 5%.
- **Cut before you add.** When a feature path doesn't pull its weight, drop it. The framework should always be getting smaller relative to its capability surface.

## Stack stance

- **gRPC-driven, protobuf-driven.** This is the design center. Connect-Web for browser clients, gRPC for service-to-service, MCP for AI agents, CLI for shell. There is no first-class REST surface — REST routes are only consumed (via `connect_http` proxying) and never served.
- **No legacy compat.** Modern-forward. We pick one format and update tests instead of preserving the old one. Examples: Connect-style errors only (lowercase, unwrapped), `application/proto` only (no `application/x-protobuf`), Python is async-only.

## Code style

- **No micro-optimizations.** The binary marshal/unmarshal path in `invoke()` for converting `dynamicpb.Message` to typed protos IS justified — it's the hot path for gRPC proxying and the alternative (JSON round-trip) is measurably slower. But don't add similar optimizations elsewhere without a clear need.
- **No unnecessary abstractions.** Three similar lines of code is better than a premature helper function.
- **Tests should test behavior, not properties.** Consolidate granular property-check tests into comprehensive behavior tests. A single `TestParseServices` that checks name, comment, methods, and types is better than 5 separate tests each checking one field.
- **Error handling should be practical.** Don't add validation for things that can't happen. Trust internal code paths.

## Things to know

### Go `Serve()` with multiple projections
When serving multiple projections, goroutines run in parallel and the first error causes `Serve()` to return. Other projection goroutines will continue running as the process exits. This is fine — these are long-running servers meant to run until process termination.

### MCP protocol compliance
MCP uses JSON-RPC 2.0 over stdio. Key rules:
- Requests without `id` are notifications — no response
- Parse errors return a response with `null` id and error code `-32700`
- Method not found returns error code `-32601`

### Include/Exclude filtering
Both Go and Python support glob-based filtering of which methods get registered:
- `server.Include("*.Greet")` / `server.include("*.Greet")`
- `server.Exclude("*Poll*")` / `server.exclude("*Poll*")`
- Environment variables: `INVARIANT_INCLUDE`, `INVARIANT_EXCLUDE` (comma-separated)
- `*` matches any characters including dots
- Exclude is applied after include

### HTTP is Connect-only
The HTTP projection serves only the canonical Connect route: `POST /{package.Service}/{Method}`. Body is `application/json` or `application/proto`. There is no server-side `google.api.http` REST routing — those annotations are still read by the `connect_http` *client* for proxying to legacy REST APIs we don't own.

### HTTP error format
Connect-style envelope only: `{"code": "invalid_argument", "message": "...", "details": [...]}`. Lowercase code, no wrapper, no toggle. The `connect_http` client is tolerant — accepts both this format and the legacy wrapped `{"error": {...}}` format from remote services.

### Tool catalog and descriptor endpoints
- `GET /` and `GET /__invariant/tools` → `{"tools": [...]}` (same shape as MCP `tools/list`).
- `GET /__invariant/descriptor.binpb` → raw FileDescriptorSet bytes for tooling.

### gRPC reflection
Always registered. `grpcurl`, Buf Studio, and Connect debug clients work without extra setup. Don't gate this — it's table stakes for gRPC-driven workflows.

### Validation
`invariant.Validation()` (Go) / `invariant.validation()` (Python) — opt-in interceptor running `protovalidate`. Failures short-circuit with `invalid_argument` plus field-level `BadRequest` details.

### Performance targets
Both languages hit ~1 µs for direct `Invoke()`. The HTTP path stays under 30 µs (Go) and 600 µs (Python — uvicorn + httpx overhead dominates). The HTTPProto path caches descriptors and the typed `reflect.Type` at `HTTPHandler()` build time so per-request work stays minimal — never call `protodesc.NewFiles` per request. See `go/benchmarks_test.go` and `python/bench/bench.py`.

### Proto descriptor requirement
`buf build --include-source-info -o descriptor.binpb` — the `--include-source-info` flag is critical, otherwise comments won't be available for tool descriptions.

## Running

```bash
flox activate
make test      # run all tests (Go + Python)
make lint      # lint all code
make fmt       # auto-format
make generate  # regenerate proto stubs
```

## Dependency boundaries

Three lockfiles, three sources of truth — keep them clean:

- **`.flox/env/manifest.toml`** — language toolchains and CLI tools only: `python3`, `uv`, `go`, `buf`, `golangci-lint`, `ruff`, `protoc`, `protoc-gen-go`. Do NOT install Python or Go libraries here.
- **`python/pyproject.toml` + `python/uv.lock`** — every Python runtime and dev dep. `uv run` resolves against this.
- **`go/go.mod` + `go/go.sum`** — every Go dep.

CI (`.github/workflows/ci.yml`) runs everything inside `flox activate`, so contributors and CI hit the same toolchain by construction.

## Not yet implemented

- `response_body` mapping in server-side HTTP handler (client-side works)
- Full path-template grammar beyond `{field}`, `{field=*}`, `{field=**}`
- Client-side selection among `additional_bindings`
- Streaming RPC support (only unary RPCs are projected)
