# Invariant Protocol

**A gRPC-first, opinionated, thin framework.** Write one `.proto` — the gRPC contract — and the framework projects it onto MCP, CLI, HTTP/Connect, and gRPC itself. Every projection is a translation of the same gRPC service: same method paths, same error codes, same interceptor model. gRPC is the foundation; the other surfaces are views.

```protobuf
// Manages user accounts.
service UserService {
  // Create a new user account. Returns the created user with a generated ID.
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse);
}

message CreateUserRequest {
  // Display name shown in the UI, e.g. "Alice Smith".
  string display_name = 1;

  // Email address. Must be unique across all accounts.
  string email = 2;

  // Account role. Determines permissions.
  Role role = 3;

  // Optional tags for organizing users, e.g. ["engineering", "london"].
  repeated string tags = 4;
}

// Account permission levels.
enum Role {
  ROLE_UNSPECIFIED = 0;
  // Read-only access to dashboards and reports.
  ROLE_VIEWER = 1;
  // Can create and modify resources.
  ROLE_EDITOR = 2;
  // Full access including user management.
  ROLE_ADMIN = 3;
}
```

Those comments — the ones you'd write anyway for good documentation — are all an AI agent needs to discover, understand, and call your service. The types, enums, and descriptions flow directly into JSON Schema that any LLM can read. Invariant projects your services into MCP tools, CLI commands, HTTP endpoints, and gRPC servers. Same comments, same types, same code. Zero glue.

## Why gRPC-first

The framework treats **gRPC as the canonical surface** and every other protocol as a translation of it:

- **Source of truth**: the proto descriptor. Service names, method names, types, comments — all gRPC's vocabulary.
- **Method paths**: `/{package.Service}/{Method}`. gRPC's canonical path shape. Connect uses the same, by spec. MCP uses `ServiceName.MethodName` which is the same identity in dot-notation.
- **Error codes**: grpc-go `codes.Code` and `status.Status`, Python `grpc.StatusCode`, and Rust's tonic code equivalent. Connect's HTTP error envelope translates these by name. MCP wraps them. CLI propagates them.
- **Interceptor model**: `UnaryServerInterceptor` / `StreamServerInterceptor` — same signatures gRPC uses. First registered = outermost, like gRPC.
- **Reflection**: `grpc.reflection.v1.ServerReflection` is registered by the runtime implementations. `grpcurl`, Buf Studio, and Connect debug clients work out of the box.
- **Deadlines + cancellation**: gRPC-native via context (Go / Rust) / asyncio (Python). Connect-Timeout-Ms is just the HTTP carrier for the same deadline.

ConnectRPC is the **first-class HTTP translation** of gRPC — same semantics, just over plain HTTP/1.1 or HTTP/2 instead of strict HTTP/2-with-prior-knowledge. Go, Python, and Rust hand-roll the small Connect protocol surface so the same descriptor-driven dispatch drives every projection. TypeScript uses Connect-ES (`@connectrpc/connect` + `@connectrpc/connect-node`) because its router accepts runtime Protobuf-ES descriptors, so we keep the no-generated-server-stubs design while using the modern Node Connect stack.

MCP and CLI are also gRPC translations: MCP `tools/call` is unary gRPC dispatch with a JSON envelope; CLI is unary or streaming gRPC dispatch with stdout output. Switching transports is a client config change, not a code change.

```
.proto                    descriptor.binpb                your code
  │                            │                             │
  ├── buf generate → stubs     │                             │
  └── buf build ──────────────→│                             │
       (source info default)   │                             │
                               └── invariant runtime ←───────┘
                                        │
                          ┌──────┬──────┼──────┐
                          MCP    CLI   HTTP   gRPC
```

## Descriptor contract

Invariant reads tool descriptions, field descriptions, enum choices, and gRPC
reflection data from a `FileDescriptorSet`. Build it with source information
intact:

```bash
buf build -o descriptor.binpb
```

Buf 1.71 includes source information by default. Do not pass
`--exclude-source-info`: stripping proto comments from the descriptor makes
MCP/CLI/HTTP catalogs lose the descriptions agents and humans rely on.

Downstream services can enforce the comment contract in CI with the packaged
checker:

```bash
invariant-check-proto-comments descriptor.binpb
```

It checks services, RPCs, messages, fields, enums, and enum values. Common
dependency protos under `google/**` and `buf/**` are skipped by default; use
`--include` or `--exclude` globs if your descriptor contains extra imported
files.

## Your proto comments become everything

**What the AI reads** (MCP tool — auto-generated from the proto above):
```json
{
  "name": "UserService.CreateUser",
  "description": "Create a new user account. Returns the created user with a generated ID.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "display_name": {
        "type": "string",
        "description": "Display name shown in the UI, e.g. \"Alice Smith\"."
      },
      "email": {
        "type": "string",
        "description": "Email address. Must be unique across all accounts."
      },
      "role": {
        "type": "string",
        "enum": ["ROLE_UNSPECIFIED", "ROLE_VIEWER", "ROLE_EDITOR", "ROLE_ADMIN"],
        "description": "Account role. Determines permissions."
      },
      "tags": {
        "type": "array",
        "items": {"type": "string"},
        "description": "Optional tags for organizing users, e.g. [\"engineering\", \"london\"]."
      }
    },
    "required": ["display_name", "email", "role"],
    "additionalProperties": false
  }
}
```

**What the human reads** (CLI `--help`):
```
  UserService CreateUser
    Create a new user account. Returns the created user with a generated ID.
    Fields:
      display_name         string                          (required)  — Display name shown in the UI, e.g. "Alice Smith".
      email                string                          (required)  — Email address. Must be unique across all accounts.
      role                 ROLE_VIEWER|ROLE_EDITOR|ROLE_ADMIN (required)  — Account role. Determines permissions.
      tags                 array<string>                               — Optional tags for organizing users, e.g. ["engineering", "london"].
```

**What the browser calls** (HTTP):
```
POST /user.v1.UserService/CreateUser
{"display_name": "Alice Smith", "email": "alice@example.com", "role": "ROLE_EDITOR", "tags": ["engineering"]}
```

Write the comment once. It appears everywhere — typed, validated, with enums, required fields, and descriptions.

## Python API

Python spelling is shown here. Go, Rust, and TypeScript keep shape-mirrored
APIs where their runtimes support the same surface; the Python `connect_http`
transport-policy surface is slated for Go/Rust parity in a follow-up.

```
from_descriptor("descriptor.binpb")   # load proto descriptor
from_bytes(raw_bytes)                 # load from embedded bytes

register(servicer)                    # wire your implementation (Python: async)
connect("host:port")                  # or proxy to a remote gRPC server (unary only)
connect_http("https://api.example",    # or proxy to a remote HTTP service (unary only)
    auth=..., service_config=..., options=..., observer=...)

use(interceptor)                      # add middleware on unary RPCs
use_stream(interceptor)               # add middleware on server-streaming RPCs

invoke(name, request)                 # in-process unary dispatch by tool name
invoke_stream(name, request)          # in-process server-streaming dispatch
asgi_app() / HTTPHandler()            # mount on an existing HTTP server
serve(...)                            # start projections (blocking)
stop()                                # cleanup
```

The implementations are **shape mirrors** — same surface where supported, idiomatic per language. Python and TypeScript are async-native (sync handlers are rejected at `register()` time). Go stays sync. There is no async/sync compatibility layer.

## Go

```go
type GreetServer struct {
    greetpb.UnimplementedGreetServiceServer
}

func (*GreetServer) Greet(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
    return &greetpb.GreetResponse{Message: "Hi " + req.GetName()}, nil
}

srv, err := invariant.ServerFromDescriptor("descriptor.binpb")
if err != nil { log.Fatal(err) }
greetpb.RegisterGreetServiceServer(srv, &GreetServer{})

lis, err := net.Listen("tcp", ":50051")
if err != nil { log.Fatal(err) }

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
defer stop()
go func() {
    <-ctx.Done()
    srv.GracefulStop()
}()

if err := srv.Serve(lis); err != nil { log.Fatal(err) }
```

`*invariant.Server` implements `grpc.ServiceRegistrar`, so generated
`Register<Service>Server` functions are the primary registration API. Native
gRPC owns the main lifecycle. Optional projections reuse that same registered
implementation. If the process needs only projections, run them instead of
`Serve`:

```go
err := srv.ServeProjections(ctx, invariant.HTTP(8080), invariant.MCP())
// Or run one CLI invocation from os.Args:
err = srv.ServeProjections(ctx, invariant.CLI())
```

The first Go example serves native gRPC only. To serve native gRPC and optional
projections together, register everything first and then run the two lifecycle
methods concurrently:

```go
go func() {
    if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
        log.Printf("gRPC: %v", err)
    }
}()
if err := srv.ServeProjections(ctx, invariant.HTTP(8080), invariant.MCP()); err != nil {
    log.Printf("projection: %v", err)
}
```

`ServeProjections` honors context cancellation. `ServeGRPC` is an explicit
alias for `Serve`; `GRPCServer` exposes the owned `*grpc.Server`, and
`GetServiceInfo` mirrors grpc-go service discovery. Calling `Serve`,
`ServeProjections`, `HTTPHandler`, `Invoke`, `InvokeStream`, or `GRPCServer`
freezes service registration and all configuration. Late generated registration
panics with a deterministic Invariant error, matching grpc-go's setup-time
registration contract. Call `GracefulStop` to drain native gRPC requests, or
`Stop` for an immediate shutdown.

The reflection-based `Register` method and `Connect` spelling remain as
compatibility wrappers over the same registration model. The old
`GRPC(port, ...)` projection is deprecated: create a listener and call `Serve`,
and pass native `grpc.ServerOption` values to the Invariant constructor.
Projection-level gRPC options are rejected instead of being silently applied
to a different server.

## Python (async-native)

```python
import asyncio

class GreetServicer:
    async def Greet(self, request, context):
        return GreetResponse(message=f"Hi {request.name}")

server = Server.from_descriptor("descriptor.binpb")
server.register(GreetServicer())

asyncio.run(server.serve(mcp=True))                # MCP over stdio
asyncio.run(server.serve(cli=True))                # CLI from sys.argv
asyncio.run(server.serve(http=8080))               # HTTP server (ASGI on uvicorn)
asyncio.run(server.serve(grpc=50051))              # gRPC server (grpc.aio)
asyncio.run(server.serve(http=8080, grpc=50051))   # multiple at once
```

Sync handlers are rejected at `register()`:

```python
class BadServicer:
    def Greet(self, request, context):  # sync — TypeError at register time
        ...
```

## Mount on an existing HTTP server

If you already run an ASGI app or `http.Server`, mount the projection instead of binding a new port:

```python
# Python — invariant.asgi_app() returns an ASGI callable
app = server.asgi_app()
# e.g. mount on Starlette/FastAPI:
asgi_application.mount("/inv", app)
```

```go
// Go — server.HTTPHandler() returns an http.Handler
mux := http.NewServeMux()
h, _ := server.HTTPHandler()
mux.Handle("/inv/", http.StripPrefix("/inv", h))
```

## Programmatic invocation

Dispatch by tool name without spinning up a projection — useful for in-process workflow runtimes and tests.

```go
// Unary
resp, err := server.Invoke(ctx, "GreetService.Greet", &GreetRequest{Name: "World"})

// Streaming — send is invoked once per emitted message
err = server.InvokeStream(ctx, "GreetService.Watch", &WatchRequest{...},
    func(msg proto.Message) error { fmt.Println(msg); return nil })
```

```python
# Unary
resp = await server.invoke("GreetService.Greet", GreetRequest(name="World"))

# Streaming
async for msg in server.invoke_stream("GreetService.Watch", WatchRequest(...)):
    print(msg)
```

## Remote proxy (zero implementation)

Don't have the source? Point at a running gRPC server:

```go
srv, _ := invariant.ServerFromDescriptor("descriptor.binpb")
conn, _ := grpc.NewClient(
    "localhost:50051",
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
defer conn.Close() // the caller owns the connection

srv.ConnectGRPC(conn, grpc.WaitForReady(true))
srv.ServeProjections(ctx, invariant.MCP()) // the remote service is now MCP tools
```

`ConnectGRPC` accepts any `grpc.ClientConnInterface` plus default
`grpc.CallOption` values. The caller owns the connection; request deadlines,
cancellation, metadata, status details, and response headers/trailers flow
through the proxy. The remote methods are registered on the same native gRPC
server as local implementations, so `srv.Serve(listener)` also exposes them to
ordinary generated clients. Streaming proxy methods remain intentionally
excluded.

Remote HTTP service (configure outbound headers before connecting):

```go
srv, _ := invariant.ServerFromDescriptor("descriptor.binpb")
srv.UseHTTPHeaderProvider(func(ctx context.Context, req *invariant.OutboundHTTPRequest) (map[string]string, error) {
    // Example only: replace with real signing logic.
    return map[string]string{"X-Example-Signature": sign(req.Method, req.URL, req.Body)}, nil
})
srv.ConnectHTTP("https://api.example.com")
srv.ServeProjections(ctx, invariant.MCP()) // your HTTP service is now an MCP tool
```

```python
from invariant import HTTPAuth

server = Server.from_descriptor("descriptor.binpb")

def sign_headers(req):
    # Example only: replace with real signing logic.
    return {"X-Example-Signature": sign(req.method, req.url, req.body)}

server.connect_http(
    "https://api.example.com",
    auth=HTTPAuth(header_provider=sign_headers),
)
server.serve(mcp=True)  # your HTTP service is now an MCP tool
```

## Remote HTTP proxy (`connect_http`)

When the remote service is a plain REST API (not yours), `connect_http` reads `google.api.http` annotations on your local `.proto` to know how to call it. Server-side, Invariant only exposes Connect — REST routes are not generated.

```protobuf
import "google/api/annotations.proto";

service UserService {
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse) {
    option (google.api.http) = {
      post: "/v1/users"
      body: "*"
    };
  }
}
```

Add the dep in `buf.yaml`:

```yaml
deps:
  - buf.build/googleapis/googleapis
```

Then `buf dep update`. The client uses:

- The primary binding (or canonical fallback if no annotation).
- One pooled `httpx.AsyncClient` per Python `connect_http()` connection.
- gRPC service-config retry policy in gRPC code space:
  `max_attempts`, `initial_backoff`, `max_backoff`,
  `backoff_multiplier`, `retryable_status_codes`. A `method_config` name of
  `{}` is the gRPC wildcard default; service and method entries out-rank it.
  Status codes may be names or enum numbers, including `UNKNOWN`. Invalid
  retry config raises `ValueError` at `connect_http()` time; duplicate names,
  non-boolean `retry_unsafe_methods`, and non-integer `max_attempts` are
  rejected, and `max_attempts` is capped at gRPC's effective maximum of 5. HTTP
  `Retry-After` is treated as retry pushback: values up to `max_backoff` are
  honored exactly and reset the A6 backoff schedule, while values above
  `max_backoff` stop retrying and surface `RetryInfo` to the caller. The
  transcoding extension `retry_unsafe_methods` opts a method into retrying
  non-GET/HEAD HTTP verbs.
- Optional per-connection `HTTPAuth` providers for header and query signing.
- Optional per-connection response observer called for successful and error
  HTTP responses with raw bytes, headers, duration, and success status.
- `ChannelOptions` for receive size cap, connect/read/write/pool timeouts,
  keepalive/pool limits, proxy URL, and HTTP/2. `socks5://` proxy URLs require
  installing HTTPX's SOCKS extra in the application environment.
- `google.api.HttpBody` responses send `Accept: */*`, follow redirects, stream through
  `max_receive_message_size` while reading, and set `content_type`; response
  headers are available to the observer, not on the returned `HttpBody`.
- `INVARIANT_HTTP_HEADER_<NAME>=value` env injection (e.g. `INVARIANT_HTTP_HEADER_AUTHORIZATION="Bearer ..."`).
- Tolerant error parsing: accepts both Connect-style
  (`{"code": "invalid_argument", ...}`) and legacy wrapped
  (`{"error": {...}}`). All remote detail dicts are preserved in JSON error
  payloads; details that resolve as protobuf `Any` values are also emitted in
  gRPC rich-status trailers. `Retry-After` drives retry scheduling, while a
  remote body `RetryInfo` takes precedence in surfaced error details. HTTP 502
  maps to `UNAVAILABLE`.

## Server-streaming

Declare `stream` in the `.proto` and write the handler as a yield/Send loop. Every projection adapts to its native stream form — gRPC native, Connect envelope frames over HTTP, MCP `content` array, NDJSON over CLI.

```protobuf
service Tail {
  rpc Watch(WatchRequest) returns (stream Event);
}
```

```go
func (s *TailServer) Watch(req *WatchRequest, stream grpc.ServerStreamingServer[Event]) error {
    for ev := range events(req) {
        if err := stream.Send(ev); err != nil { return err }
    }
    return nil
}
```

```python
class TailServicer:
    async def Watch(self, request, context):
        async for ev in events(request):
            yield ev
```

Stream-aware middleware is registered separately with `UseStream` / `use_stream` — mirrors gRPC's split between `UnaryServerInterceptor` and `StreamServerInterceptor`. Client-streaming and bidi RPCs are intentionally not projected.

## MCP over HTTP

If you're already serving the HTTP projection, MCP is also available at `POST /mcp` (Streamable HTTP transport): one JSON-RPC request per POST, one JSON-RPC response back. Use it to point Claude.ai, agent platforms, and any remote MCP client at your server without a stdio process. The stdio MCP transport (`MCP()` / `mcp=True`) still works for local clients.

## Middleware

Go uses grpc-go's interceptor types directly. Register shared middleware with
`Use` and `UseStream`; it runs once per call across native gRPC, HTTP, MCP, and
CLI. First registered is outermost. Services registered through generated
`Register<Service>Server` functions retain their concrete generated request and
response types inside these interceptors.

```go
var logging grpc.UnaryServerInterceptor = func(
    ctx context.Context,
    req any,
    info *grpc.UnaryServerInfo,
    handler grpc.UnaryHandler,
) (any, error) {
    log.Printf("→ %s", info.FullMethod)
    resp, err := handler(ctx, req)
    log.Printf("← %s err=%v", info.FullMethod, err)
    return resp, err
}
srv.Use(logging)

var streamLogging grpc.StreamServerInterceptor = func(
    service any,
    stream grpc.ServerStream,
    info *grpc.StreamServerInfo,
    handler grpc.StreamHandler,
) error {
    log.Printf("↔ %s", info.FullMethod)
    return handler(service, stream)
}
srv.UseStream(streamLogging)
```

Interceptors passed as constructor `grpc.ServerOption` values apply only to
native gRPC:

```go
srv, _ := invariant.ServerFromDescriptor(
    "descriptor.binpb",
    grpc.ChainUnaryInterceptor(nativeOnly),
    grpc.ChainStreamInterceptor(nativeStreamOnly),
)
```

Constructor interceptors and shared interceptors each execute exactly once.
Registering the same interceptor in both places deliberately executes it twice;
Invariant never silently duplicates it.

```python
async def logging_interceptor(request, context, info, handler):
    print(f"→ {info.full_method}")
    response = await handler(request, context)
    print(f"← {info.full_method}")
    return response

server.use(logging_interceptor)
```

## Projections

| Projection | What happens |
|------------|-------------|
| **MCP** | Each RPC becomes a tool. JSON Schema from proto types. Descriptions from proto comments. Served over stdio, or over HTTP at `POST /mcp` (Streamable HTTP transport — single JSON-RPC request per call). |
| **CLI** | `ServiceName Method -r request.json`. AI agents and humans alike call RPCs from the shell. `--help` lists tools with field types. Streaming RPCs emit newline-delimited JSON. |
| **HTTP** | Connect-only. Unary: `POST /{package.Service}/{Method}` with `application/json` or `application/proto`. Server-streaming: same path with `application/connect+json` or `application/connect+proto` (one request envelope → stream of response envelopes + end-stream marker). In Go, encoded requests, responses, and individual stream messages default to 16 MiB limits. `GET /` returns the tool catalog. `GET /healthz` (and `/readyz`) returns `{"status":"ok"}` for k8s probes. `GET /__invariant/descriptor.binpb` returns the raw FileDescriptorSet. |
| **gRPC** | Standard native gRPC with reflection enabled by default. Go owns a canonical `grpc.Server`, uses generated registration, and supports normal grpc-go clients and semantics. |

Go `Include` / `Exclude` filters control optional projection catalogs only.
They do not remove methods from the canonical generated native gRPC service.

Go exposes separate HTTP wire limits through `SetMaxUnaryRequestBytes`,
`SetMaxUnaryResponseBytes`, `SetMaxStreamRequestBytes`, and
`SetMaxStreamResponseBytes`; `ConfigureMethod` overrides any of them for one
full method. Streaming limits apply independently to each encoded message, not
the lifetime of the stream. Exceeding a limit returns `resource_exhausted`.
These are HTTP JSON/protobuf byte limits: opaque options such as
`grpc.MaxRecvMsgSize` and `grpc.MaxSendMsgSize` govern native gRPC messages and
do not automatically govern HTTP bytes. Connect end-stream control data has a
separate 1 MiB safety bound, so unbounded error details or trailers cannot
bypass the per-message limits.

In Go, HTTP headers are untrusted. By default, only `traceparent`, `tracestate`,
`baggage`, and `x-request-id` become incoming gRPC metadata.
`UseHTTPMetadataMapper` can select additional reviewed values, but Invariant
still removes authorization, tenant, principal, role, user, protocol, and
internal metadata keys. Authenticate in HTTP middleware and place trusted
identity in the request's incoming-metadata context; never let callers assert
identity by choosing a header. The `invariant-internal-*` namespace is reserved
for trusted middleware and is never accepted from an HTTP mapper.
Safe handler metadata set with `grpc.SetHeader`, `grpc.SendHeader`, or
`grpc.SetTrailer` is mapped back to Connect response headers or the streaming
end envelope.

### Connect protocol

HTTP errors use the Connect error envelope:

```json
{
  "code": "invalid_argument",
  "message": "proto: (line 1:27): unknown field \"extra\"",
  "details": [
    {
      "type": "google.rpc.BadRequest",
      "value": "<base64-encoded protobuf bytes>"
    }
  ]
}
```

Code names follow Connect's lowercase convention (`invalid_argument`,
`not_found`, etc.). Rich detail `value` fields use unpadded base64 of the
serialized protobuf detail.

### Validation

Protovalidate constraints on your proto enforced via opt-in interceptors:

```go
v, _ := invariant.Validation()
server.Use(v)

vs, _ := invariant.ValidationStream()       // server-streaming RPCs
server.UseStream(vs)
```

```python
server.use(invariant.validation())
server.use_stream(invariant.validation_stream())   # server-streaming RPCs
```

Constraint violations short-circuit with `invalid_argument` and field-level `BadRequest` details. Streaming variants validate the request before the response stream opens, so failures never produce any messages.

### gRPC reflection and health

The native gRPC server registers reflection automatically. Try it:

```bash
grpcurl -plaintext localhost:50051 list
grpcurl -plaintext -d '{"name":"World"}' localhost:50051 greet.v1.GreetService/Greet
```

Health is conventional grpc-go registration rather than synthesized
application state:

```go
healthServer := health.NewServer()
healthpb.RegisterHealthServer(srv, healthServer)
healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
```

### Production behavior

- **Deadlines.** HTTP honors the Connect-Timeout-Ms request header — the server cancels in-flight work once the deadline elapses and returns `deadline_exceeded`. gRPC honors gRPC-native deadlines via `context.Context`.
- **Cancellation.** MCP supports `notifications/cancelled` — clients can interrupt long-running `tools/call` invocations. `tools/call` runs concurrently so notifications are processed without waiting for the tool to finish; metadata methods (`initialize`, `tools/list`, `ping`) stay strictly ordered.
- **Tool-name collisions.** If two services in different packages share the same simple name, registration fails with a clear error rather than silently overwriting.
- **Unknown tools.** `Invoke()`/`invoke()` returns a `NOT_FOUND` status error that maps to the right code through every projection.
- **Idempotent shutdown.** `stop()` is safe to call multiple times and before `serve()`.

## Install

Invariant-owned packages are distributed only from Git. They are not published
to PyPI, the npm registry, crates.io, or another language registry. Release
examples use the shared root tag; production builds that require a commit-level
pin can use each tool's commit-revision syntax. The language package managers
below build and install that Git source—they are not registry distribution
channels for Invariant. Third-party dependencies still resolve through their
normal ecosystem registries or proxies. Go module proxies may transparently
cache public Git revisions during normal `go get` resolution; no separate Go
artifact is published.

All four language packages use the version in `VERSION` and ship together from
one repository tag. Release `0.6.1`, for example, uses only the tag `v0.6.1`.
The retired `go/v*` tags apply only to the historical nested Go module.

**Go:**
```bash
go get github.com/jim-technologies/invariantprotocol/go@v0.6.1
```

This follows Go's conventional one-module-per-repository layout: `go.mod` is at
the repository root and the Go package lives in `go/`. Code imports
`github.com/jim-technologies/invariantprotocol/go`, while a consumer's `go.mod`
records the containing module as
`github.com/jim-technologies/invariantprotocol v0.6.1`. The `/go` suffix is a
package directory, not a separate version or tag namespace.

**Python:**
```bash
pip install "invariant-protocol @ git+https://github.com/jim-technologies/invariantprotocol.git@v0.6.1#subdirectory=python"
```

**Rust:**
```toml
# Cargo.toml
[dependencies]
invariant-protocol = { git = "https://github.com/jim-technologies/invariantprotocol", tag = "v0.6.1" }
```

The crates.io package named `invariant-protocol` belongs to an unrelated
project. Use the Git dependency above.

**TypeScript (source package):**
```bash
npm install --allow-git=root "github:jim-technologies/invariantprotocol#v0.6.1"
```

For a full commit pin, replace `@v0.6.1` with `@COMMIT_SHA` in the Go and
Python commands, use `rev = "COMMIT_SHA"` instead of `tag` for Cargo, and use
`#COMMIT_SHA` for npm.

## TypeScript

The TypeScript runtime is descriptor-driven like the other implementations and uses `@bufbuild/protobuf` for dynamic messages. Its HTTP/RPC projection is powered by Connect-ES, not a custom Connect implementation. It supports local servicer registration, remote gRPC proxying via `connect()`, remote HTTP proxying via `connectHttp()`, unary and server-streaming `invoke`, unary and stream interceptors, JSON Schema tool catalogs, CLI helpers, MCP dispatch including `POST /mcp`, Node HTTP/Connect (`application/json`, `application/proto`, `application/connect+json`, `application/connect+proto`), and native grpc-js serving with reflection.

`connectHttp()` supports the primary `google.api.http` binding plus canonical fallback, dynamic header/query auth hooks, response observers, and receive/deadline caps. Python still has the richest HTTP retry and `google.api.HttpBody` handling.

```ts
import { Server, serveHttp } from "@jim-technologies/invariant-protocol";

class GreetServicer {
  async Greet(request) {
    return { message: `Hi ${request.name}` };
  }

  async *StreamGreet(request) {
    for (let i = 0; i < (request.count || 1); i += 1) {
      yield { message: `Hi ${request.name} #${i + 1}` };
    }
  }
}

const server = Server.fromDescriptor("descriptor.binpb");
server.register(new GreetServicer());
await serveHttp(server, 8080);

// Or expose gRPC with reflection:
await server.serveGrpc(50051);

// Or proxy an existing service:
server.connect("localhost:50051");
server.connectHttp("https://api.example.com");
```

## Rust

The Rust implementation is descriptor-driven like the Go and Python ones — no per-service codegen, no generated traits. We do **not** depend on the `connectrpc` crate because its model is codegen-per-service, which conflicts with this stance. The Connect protocol is hand-rolled on top of `axum` + `tower`, exactly the way the Go and Python projections are.

```rust
use invariant::{Server, Status, projections::http::serve_http};
use std::sync::Arc;

// Your prost-generated types, same as you'd use with tonic.
async fn greet(req: GreetRequest) -> Result<GreetResponse, Status> {
    Ok(GreetResponse { message: format!("Hi {}", req.name), ..Default::default() })
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let server = Server::from_descriptor("descriptor.binpb")?;
    server.register_unary("GreetService.Greet", greet);
    serve_http(Arc::new(server), 8080).await?;
    Ok(())
}
```

Rust has no runtime method-name reflection, so registration is per-method (`register_unary` / `register_stream`). Every other surface matches the Go and Python implementations:

- **Unary + server-streaming RPCs** (`register_unary`, `register_stream`, `ServerStreamTx<T>`)
- **HTTP / Connect** — unary `application/json` + `application/proto`, streaming `application/connect+json` + `application/connect+proto`, envelope framing, `Connect-Timeout-Ms`, 16 MiB body cap
- **MCP** — stdio transport + `POST /mcp` HTTP transport, `tools/list` with `_meta.streaming`, streaming `tools/call` collects chunks into the content array
- **CLI** — `cli_write(server, args, writer)`, real-time flush per chunk on streams
- **gRPC** — descriptor-driven dispatch via `Routes::from(axum::Router)` (tonic 0.14), HTTP/2 prior-knowledge via `tonic::transport::Server::add_routes`

Streaming, unary interceptors, and stream interceptors all flow through `use_interceptor` / `use_stream_interceptor` with the same first-registered-outermost ordering as the Go and Python ports.

**No middleware ships in Rust beyond the dispatch core.** The framework's stance is thin: validation, auth, observability, retries — users compose their own interceptors and call whatever libraries they want (`protovalidate-rs`, `tower-http`, `tracing`, etc.). Go and Python ship a `validation()` interceptor for protovalidate; Rust intentionally doesn't, because shipping a stub that doesn't validate is misleading and shipping a real wrapper would pull a new dep.

## Requirements

- `buf build -o descriptor.binpb` (source info is included by default)
- Generated stubs for your language (`buf generate`)
- For `connect_http` proxy mode with `google.api.http`, add Buf dep `buf.build/googleapis/googleapis`
- For `Validation()` / `validation()`, add Buf dep `buf.build/bufbuild/protovalidate`

Go service protos need both message and gRPC generation. A minimal Buf v2
configuration includes:

```yaml
version: v2
plugins:
  - protoc_builtin: go
    out: gen
    opt: paths=source_relative
  - remote: buf.build/grpc/go:v1.6.2
    out: gen
    opt: paths=source_relative
```

## Benchmarks

Reference numbers, Intel Xeon E5-2696 v4 (Linux, Go 1.25, Python 3.14, Rust 1.94):

| Path                                  | Go             | Python          | Rust            |
|---------------------------------------|----------------|-----------------|-----------------|
| Direct `Invoke()`                     | 1.6 µs / op    | 2.0 µs / op     | 0.8 µs / op     |
| Direct `InvokeStream()` (10 msgs)     | 3.7 µs / call  | —               | 42.9 µs / call  |
| Direct `InvokeStream()` (per chunk)   | ~366 ns        | —               | ~4.3 µs         |
| HTTP JSON unary                       | 298 µs / op    | 1677 µs / op    | 206 µs / op     |
| HTTP binary proto unary               | 269 µs / op    | 1641 µs / op    | 199 µs / op     |
| gRPC unary                            | 302 µs / op    | 509 µs / op     | (pending)       |

Reading: Rust is fastest on unary (direct + HTTP). Go is fastest per-chunk on streaming because the framework uses a sync callback — Rust's streaming hop is an `mpsc` channel send, which costs ~4 µs/chunk but decouples producer + consumer cleanly across projections. For typical LLM-token-stream workloads (~50 tok/s) the per-chunk overhead is negligible.

Run yourself:

```bash
go test -bench=. -benchtime=2s -run=^$ ./...
cd python && uv run python bench/bench.py
cd rust && cargo bench --bench bench
```

## Deployment

The framework ships **no first-class TLS helper**, because every language's
gRPC/HTTP stack already has a battle-tested one. Wire them up at the
projection boundary:

**Go** — pass ordinary `grpc.ServerOption` values to the Invariant constructor,
register generated services, then serve a listener you own:

```go
creds, _ := credentials.NewServerTLSFromFile("cert.pem", "key.pem")
srv, _ := invariant.ServerFromDescriptor(
    "descriptor.binpb",
    grpc.Creds(creds),
    grpc.MaxRecvMsgSize(16<<20),
)
greetpb.RegisterGreetServiceServer(srv, &GreetServer{})

lis, _ := net.Listen("tcp", ":50051")
srv.Serve(lis)
```

For the HTTP/Connect projection, mount the returned `http.Handler` on your
own `http.Server` configured with `TLSConfig`:

```go
h, _ := srv.HTTPHandler()
s := &http.Server{Addr: ":443", Handler: h, TLSConfig: tlsCfg}
s.ListenAndServeTLS("", "") // certs already in TLSConfig
```

**Python** — `Server.serve(grpc=...)` is a thin wrapper. For TLS, build the
gRPC server directly via the lower-level helper:

```python
from invariant.projections.grpc import build_grpc_server
grpc_server = build_grpc_server(server, options=[("grpc.max_concurrent_streams", 100)])
creds = grpc.ssl_server_credentials([(key, cert)])
grpc_server.add_secure_port("[::]:50051", creds)
await grpc_server.start()
```

For HTTP, mount `server.asgi_app()` on your own ASGI server (`uvicorn`,
`hypercorn`) with TLS settings configured there.

**Rust** — `serve_grpc` is a convenience over tonic; for TLS go through tonic
directly:

```rust
let tls = tonic::transport::ServerTlsConfig::new().identity(identity);
tonic::transport::Server::builder()
    .tls_config(tls)?
    .add_routes(invariant::projections::grpc::grpc_routes(server))
    .serve(addr)
    .await?;
```

For HTTP, mount `http_router(server)` inside your axum app and serve via
`axum_server::tls_rustls` or `axum::serve` behind a TLS-terminating proxy
(envoy, nginx, AWS ALB) — whichever your deployment shape already uses.

### Process management + restarts

Go's `ServeProjections(ctx, ...)`, Python's `server.serve(...)`, and Rust's
`projections::serve::serve(...)` honor a cancellation signal. Native Go gRPC
uses `Serve(listener)` with `GracefulStop()` to drain in-flight requests. Hook
both forms to your supervisor's TERM signal handler.

### Reverse-proxy compatibility

Connect's HTTP/1.1 + HTTP/2 wire formats pass through any standard L7 proxy
(envoy, nginx, Cloudflare, ALB). Native gRPC requires HTTP/2 end-to-end —
envoy is the usual choice. MCP stdio runs as a child process of the agent
host (Claude.ai, Cursor, etc.); no proxy involved.

## License

Apache 2.0
