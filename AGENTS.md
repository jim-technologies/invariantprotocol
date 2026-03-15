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

## Convention over configuration

- We do NOT support extensive configurability. Support common use cases well.
- Don't add feature flags, options structs, or builder patterns for hypothetical needs.
- If something works for 95% of cases, ship it. Don't add a knob for the other 5%.

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

### HTTP transcoding
Invariant supports `google.api.http` annotations for REST routes. The canonical RPC route (`POST /{package.Service}/{Method}`) is always available as a fallback.

### Proto descriptor requirement
`buf build --include-source-info -o descriptor.binpb` — the `--include-source-info` flag is critical, otherwise comments won't be available for tool descriptions.

## Running

```bash
make test      # run all tests (Go + Python)
make lint      # lint all code
make fmt       # auto-format
make generate  # regenerate proto stubs
```

## Not yet implemented

- `response_body` mapping in server-side HTTP handler (client-side works)
- Full path-template grammar beyond `{field}`, `{field=*}`, `{field=**}`
- Client-side selection among `additional_bindings`
- Streaming RPC support (only unary RPCs are projected)
