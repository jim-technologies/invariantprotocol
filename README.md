# Invariant Protocol

[![CI](https://github.com/jim-technologies/invariantprotocol/actions/workflows/ci.yml/badge.svg)](https://github.com/jim-technologies/invariantprotocol/actions/workflows/ci.yml)

Invariant is a gRPC-first framework for projecting one registered protobuf
service onto native gRPC, HTTP/Connect, MCP, and CLI. Application code uses
generated messages and generated service APIs; Invariant does not introduce a
second RPC model or route production calls through an in-memory gRPC hop.

Go, Python, Rust, and TypeScript are supported production runtimes, not preview
or experimental implementations. A release is blocked unless every Core row in
the cross-language feature contract has behavioral evidence in all four
runtimes. Language-specific ecosystem adapters remain explicit rather than
being mistaken for portable runtime features.

Annotated protobuf dataset messages can also compile into one versioned data
schema for Arrow, Parquet, Iceberg, and PostgreSQL. Explicit `--message`
selection remains available for controlled builds.

```text
.proto (the only authored contract)
  └─ buf build → descriptor.binpb
       ├─ buf generate → standard protobuf/gRPC bindings → application service
       │                                                    │
       ├─ invariant.Server ←──── generated registration ────┘
       │    └─ native gRPC / HTTP-Connect / MCP / CLI
       └─ invariant-schema → SchemaBundle
                              └─ Arrow / Parquet / Iceberg / PostgreSQL
```

Proto comments become MCP tool descriptions, CLI help, JSON Schema field
descriptions, and constrained enum choices. Transport and interceptor method
paths use `/{package.Service}/{Method}`; tool catalogs, CLI resolution, and
in-process invocation use the collision-free `package.Service.Method` identity.
Every JSON projection emits canonical ProtoJSON field names and scalar forms,
including explicit `json_name` overrides and decimal strings for 64-bit
integers.

## Build contract

Build the descriptor image once, then generate normal language bindings from
that exact image:

```bash
buf build -o descriptor.binpb
buf generate descriptor.binpb
```

Buf 1.71 includes source information by default. Do not pass
`--exclude-source-info`: Invariant needs those comments for discovery and tool
descriptions.

`descriptor.binpb` is a compiled artifact, not a second contract. Generated
code is produced at build time. Runtime descriptor interpretation is limited
to discovery, validation, reflection, remote proxies, and optional projections;
Invariant never generates application code on a request path.

Generated local registration is checked against the descriptor image, so stale
bindings fail during setup instead of creating different contracts on different
transports.

## Native gRPC

Native gRPC is the canonical service-to-service surface. Unary,
server-streaming, client-streaming, and bidi methods remain available there;
HTTP, MCP, and CLI intentionally expose only unary and server-streaming
methods, while remote proxies expose unary methods only.

### Go

`*invariant.Server` implements `grpc.ServiceRegistrar`. Pass ordinary
`grpc.ServerOption` values to the constructor and use the generated
registration function.

```go
type Greeter struct {
    greetpb.UnimplementedGreetServiceServer
}

func (*Greeter) Greet(
    ctx context.Context,
    req *greetpb.GreetRequest,
) (*greetpb.GreetResponse, error) {
    return &greetpb.GreetResponse{Message: "Hi " + req.GetName()}, nil
}

server, err := invariant.ServerFromDescriptor(
    "descriptor.binpb",
    grpc.MaxRecvMsgSize(16<<20),
    grpc.MaxSendMsgSize(16<<20),
)
if err != nil {
    log.Fatal(err)
}
greetpb.RegisterGreetServiceServer(server, &Greeter{})

listener, err := net.Listen("tcp", ":50051")
if err != nil {
    log.Fatal(err)
}
if err := server.Serve(listener); err != nil {
    log.Fatal(err)
}
```

`GRPCServer`, `GetServiceInfo`, `GracefulStop`, and `Stop` expose the familiar
grpc-go lifecycle. Building a projection adapter, serving, or invoking freezes
registration and configuration; late generated registration panics
deterministically because `grpc.ServiceRegistrar` cannot return an error.

### Python

Python is async-only and accepts grpcio's generated
`add_<Service>Servicer_to_server` helper directly. The caller supplies the
native port and credentials to the one returned `grpc.aio.Server`.

```python
import asyncio

from invariant import Server
import greet_pb2
import greet_pb2_grpc


class Greeter(greet_pb2_grpc.GreetServiceServicer):
    async def Greet(self, request, context):
        return greet_pb2.GreetResponse(message=f"Hi {request.name}")

    async def GreetGroup(self, request, context):
        messages = [f"Hi {person.name}" for person in request.people]
        return greet_pb2.GreetGroupResponse(messages=messages, count=len(messages))

    async def StreamGreet(self, request, context):
        for _ in range(request.count or 1):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name}")


async def main():
    server = Server.from_descriptor("descriptor.binpb")
    greet_pb2_grpc.add_GreetServiceServicer_to_server(Greeter(), server)

    native = server.grpc_server(
        options=(
            ("grpc.max_receive_message_length", 16 * 1024 * 1024),
            ("grpc.max_send_message_length", 16 * 1024 * 1024),
        )
    )
    native.add_insecure_port("[::]:50051")
    await native.start()
    await native.wait_for_termination()


asyncio.run(main())
```

Sync handlers are rejected during generated registration. For TLS, call
`add_secure_port()` with ordinary grpcio server credentials. `await
server.stop(grace=...)` drains the native server and closes Invariant-owned HTTP
clients; remote gRPC channels remain caller-owned.

### TypeScript

TypeScript uses generated Protobuf-ES `DescService` values and Connect-ES
`ServiceImpl<T>` implementations. `grpcServer()` returns one grpc-js server;
the caller binds an address and credentials.

```ts
import * as grpc from "@grpc/grpc-js";
import { Server } from "@jim-technologies/invariant-protocol";
import { GreetService } from "./gen/greet_pb.js";

const server = Server.fromDescriptor("descriptor.binpb");
server.register(GreetService, {
  greet(request) {
    return { message: `Hi ${request.name}` };
  },
});

const native = server.grpcServer({
  "grpc.max_receive_message_length": 16 * 1024 * 1024,
  "grpc.max_send_message_length": 16 * 1024 * 1024,
});
native.bindAsync(
  "0.0.0.0:50051",
  grpc.ServerCredentials.createInsecure(),
  (error) => {
    if (error) throw error;
  },
);
```

`await server.stop()` uses grpc-js graceful shutdown; `forceStop()` is the
immediate form.

### Rust

Rust uses ordinary Prost messages, generated Tonic clients and service traits,
and the small generated `register_<service>_server` bridge. The bridge is
generated at build time from the same descriptor image and retains projection
metadata without replacing Tonic's transport.

```rust
let server = std::sync::Arc::new(
    invariant::Server::from_descriptor("descriptor.binpb")?
);
greet::register_greet_service_server(&server, Greeter::default())?;

let routes = server.grpc_routes();
tonic::transport::Server::builder()
    .add_routes(routes)
    .serve("[::]:50051".parse()?)
    .await?;
```

Use `invariant-protocol-codegen` in `build.rs` to emit the Tonic bindings and
registration bridge from a decoded `FileDescriptorSet`. `grpc_routes()` also
adds reflection from that image. Tonic owns listeners, TLS, Tower layers,
message limits, and graceful shutdown. The generated
`register_<service>_server_with` form accepts a caller-supplied Tonic/Tower
wrapper for native compression, limits, and layers.

Native reflection is always enabled and publishes only the services and methods
actually served by Invariant, including registered unary remote proxies.

## Optional projections

Every optional projection calls the same registered implementation directly.

| Projection | Surface |
| --- | --- |
| HTTP/Connect | `POST /{package.Service}/{Method}` with unary JSON/protobuf and Connect server-streaming envelopes. |
| MCP | JSON-RPC tools over stdio and `POST /mcp`. |
| CLI | Unary JSON output and newline-delimited server-streaming output. |
| Discovery | `GET /`, `GET /__invariant/tools`, and `GET /__invariant/descriptor.binpb`. |
| Probes | `GET /healthz` and `GET /readyz`. |

Mount the HTTP adapter in the language's normal host:

```go
mux.Handle("/", server.HTTPHandler())
```

```python
asgi_app = server.asgi_app()  # mount in an ASGI host
```

```ts
const handler = server.httpHandler();
createServer((request, response) => {
  void handler(request, response);
}).listen(8080);
```

```rust
let router = invariant::projections::http::http_router(server.clone());
axum::serve(listener, router).await?;
```

Go's `ServeProjections` and Python's `serve_projections` are conveniences for
optional HTTP/MCP/CLI processes. Rust exposes its projection runner and
TypeScript exposes the individual host adapters. Native gRPC keeps its own
language-standard lifecycle.

Configure `Include` / `Exclude` and their idiomatic equivalents before any
local or remote registration. They filter optional projection catalogs only
and never remove a method from a locally registered native gRPC service.
Client-streaming and bidi methods are native-gRPC only.

Per-method HTTP limits apply to the canonical Connect routes. `POST /mcp` is a
single stateless JSON-RPC request whose buffered response uses the server-wide
unary response limit; server-streaming tool collection stops as soon as that
aggregate limit would be exceeded.

### MCP Streamable HTTP

Invariant implements the non-SSE tool-server subset of MCP `2025-11-25`.
Clients send one JSON-RPC message per `POST /mcp` with
`Content-Type: application/json` and advertise both `application/json` and
`text/event-stream` in `Accept`; this implementation returns JSON, not SSE.
The initialize request must include `protocolVersion`, `capabilities`, and
`clientInfo`; if its requested version is unsupported, Invariant negotiates to
its sole supported version, `2025-11-25`. The initialize HTTP request may omit
`MCP-Protocol-Version`; later requests must send `2025-11-25`. Notifications
and client responses return `202 Accepted` with an empty body, `GET /mcp`
returns `405`, and requests with an `Origin` header are rejected by default.

Supported MCP behavior includes initialization, ping, tool discovery/calls,
and notifications. Stdio sessions implement `notifications/cancelled` for
in-flight calls. HTTP cancellation and deadlines are scoped to the current
stateless request; a separate POST cannot cancel a call from an earlier POST.
Resources, prompts, tasks, sampling, and SSE are outside the current tool-server
scope.

## Interceptors and RPC semantics

Shared middleware runs once on native gRPC and every optional projection. Native
transport middleware configured on the underlying gRPC server applies only to
native gRPC. Registering the same function in both places deliberately runs it
twice.

| Language | Shared middleware | Native-only controls |
| --- | --- | --- |
| Go | `Use(grpc.UnaryServerInterceptor)` and `UseStream(grpc.StreamServerInterceptor)` | constructor `grpc.ServerOption` interceptors |
| Python | `use(grpc.aio.ServerInterceptor)` for unary and streaming | `grpc_server(interceptors=...)` |
| Rust | `use_shared_unary` and `use_shared_stream`, with typed `tonic::Request<T>` / `Response<T>` available through the erased carrier | Tonic/Tower layers on the generated service |
| TypeScript | `use(Interceptor)` using Connect-ES's standard unary/stream interceptor | grpc-js `ServerOptions` |

For locally registered services, the shared path preserves generated request
and response types, canonical full method names, status codes and rich details,
deadlines, cancellation, request metadata, and response headers/trailers where
the host transport exposes them. Descriptor-only remote adapters may use
dynamic protobuf messages at their boundary. Unexpected handler failures
become an `internal` status instead of crashing the process.

Protovalidate adapters are available in Go, Python, and TypeScript:

```go
unary, _ := invariant.Validation()
stream, _ := invariant.ValidationStream()
server.Use(unary)
server.UseStream(stream)
```

```python
from invariant import validation

server.use(validation())
```

```ts
import { validation } from "@jim-technologies/invariant-protocol";

server.use(validation());
```

Validation failures use `invalid_argument` with `google.rpc.BadRequest`
details. Rust has no maintained framework adapter in this release; applications
can validate inside their generated Tonic service to cover every projection,
or attach a Tower layer when validation is intentionally native-gRPC-only.

## HTTP metadata and limits

HTTP headers are untrusted. The default mapper forwards only `traceparent`,
`tracestate`, `baggage`, and `x-request-id`. A custom mapper can add reviewed
application metadata, but Invariant still strips authorization, tenant,
principal, role, user, protocol, and internal identity keys. Authenticate in
the enclosing HTTP middleware; never treat a caller-chosen header as trusted
gRPC identity.

HTTP/Connect has four independent 16 MiB defaults in every runtime:

- maximum encoded unary request bytes;
- maximum encoded unary response bytes;
- maximum encoded streaming request-message bytes; and
- maximum encoded streaming response-message bytes.

Streaming limits apply per message, not to the stream lifetime. Each language
provides `SetMax*` / `set_max_*` / `setMax*` methods and a per-full-method
configuration API. Zero resets a server-wide limit to 16 MiB and makes a
per-method limit inherit from the server; negative values are rejected where
the language's integer type permits them. Exceeding a projection limit returns
`resource_exhausted`.

Native gRPC limits remain ordinary grpc-go, grpcio, Tonic, or grpc-js controls.
They measure protobuf gRPC messages and do not govern encoded HTTP JSON bytes.
`Connect-Timeout-Ms` is one absolute request deadline covering body reads,
decoding, and application dispatch; it is not restarted after decoding.

## Remote projections

Remote gRPC always accepts a caller-owned channel/client. HTTP ownership follows
the native client stack:

| Language | gRPC | HTTP |
| --- | --- | --- |
| Go | caller-owned `grpc.ClientConnInterface` via `ConnectGRPC` | server-managed client via `ConnectHTTP` |
| Python | caller-owned `grpc.aio.Channel` via `connect_grpc` | server-managed HTTPX client via `connect_http` |
| Rust | caller-owned Tonic `GrpcService` via `connect_grpc` | caller-owned Reqwest client and URL via `connect_http` |
| TypeScript | caller-owned grpc-js `Client` via `connectGrpc` | Invariant-managed Fetch adapter via `connectHttp` |

Remote gRPC projections preserve caller ownership of the connection and normal
gRPC deadlines, cancellation, metadata, rich status details, headers, trailers,
and call controls. Remote HTTP adapters preserve canonical Connect status
envelopes and bounded responses under their documented header, authentication,
and timeout policies; they are not general gRPC metadata, header, or trailer
tunnels. Remote streaming is intentionally not projected. Go, Python, and
TypeScript also understand the primary `google.api.http` binding for HTTP
transcoding; the portable remote-HTTP contract is the canonical Connect method
path.

## Protobuf-derived data schemas

Invariant compiles messages marked with `(invariant.data.v1.dataset)` into one
generated `invariant.data.v1.SchemaBundle`. The bundle is the versioned mapping
and evolution authority consumed by all four languages; there are not four
independent inference implementations.

```proto
import "invariant/data/v1/annotations.proto";

message LedgerEvent {
  option (invariant.data.v1.dataset) = {};

  optional string event_id = 1 [
    (invariant.data.v1.field) = { uuid: {} }
  ];
  optional string amount = 2 [(invariant.data.v1.field) = {
    decimal: {
      precision: 18
      scale: 4
    }
  }];
  optional bytes checksum = 3 [(invariant.data.v1.field) = {
    fixed_bytes: { byte_length: 32 }
  }];
}
```

```bash
go run ./go/cmd/invariant-schema compile \
  --descriptor descriptor.binpb \
  --output ledger.schema.binpb

go run ./go/cmd/invariant-schema arrow \
  --bundle ledger.schema.binpb --output ledger.arrow
go run ./go/cmd/invariant-schema parquet \
  --bundle ledger.schema.binpb --output ledger.parquet.schema
go run ./go/cmd/invariant-schema iceberg \
  --bundle ledger.schema.binpb --output ledger.iceberg.json
go run ./go/cmd/invariant-schema sql \
  --bundle ledger.schema.binpb --output ledger.sql
```

`compile` reads an existing output bundle before replacing it. Numeric
protobuf paths retain active field identities, removed identities remain
tombstoned, field storage names survive same-number source renames while the
dataset full name remains stable, and incompatible reuse fails.
Repeated `--message` flags can explicitly select roots for controlled or
one-off builds; annotation discovery is the normal convention. Commit and
review the bundle, but continue to author only protobuf.

Portable field refinements map canonical decimal text, canonical UUID text,
and exact-width bytes to native Arrow, Parquet, Iceberg, and PostgreSQL types.
Annotations declare those value domains; they do not validate message values by
themselves. Python's `arrow_table()` currently enforces the canonical values,
and other writers must validate at their own boundary. The options do not
encode keys, indexes, partitions, placement, or migration policy.

The PostgreSQL renderer emits desired-state DDL directly for Atlas; HCL is not
another source format. Atlas consumes the generated `file://` SQL desired state
using a PostgreSQL development database. CI applies that SQL to disposable
PostgreSQL 18.4, inspects the result, and requires a zero diff. Invariant does
not infer keys, indexes, partitions, catalog operations, file layout, or
migrations, and it does not apply production database changes.

Python's optional data adapter converts a bundle dataset and matching generated
messages into a real `pyarrow.Table`:

```python
import pyarrow.parquet as pq
from invariant import arrow_table, find_dataset, parse_schema_bundle

bundle = parse_schema_bundle(open("ledger.schema.binpb", "rb").read())
dataset = find_dataset(bundle, "ledger.v1.LedgerEvent")
if dataset is None:
    raise ValueError("dataset is not present in the bundle")
table, diagnostics = arrow_table(dataset, events)
pq.write_table(table, "ledger.parquet")
```

PyArrow owns Parquet file writing. See [the data-schema contract](docs/data-schemas.md)
for mappings, diagnostics, evolution rules, and target limitations.

## Install

Invariant-owned packages are distributed only from Git. They are not published
to PyPI, the npm registry, crates.io, or another language registry. Every
language package and the Rust codegen crate share `VERSION` and the single root
tag `v0.9.0`; new releases do not create language-prefixed tags. The project
follows Semantic Versioning; while it remains below 1.0, minor releases may
refine the public API without weakening documented wire guarantees.

Go:

```bash
go get github.com/jim-technologies/invariantprotocol/go@v0.9.0
```

The repository is one Go module. `/go` is the package directory, so consumers
import `github.com/jim-technologies/invariantprotocol/go` while `go.mod`
records the root module revision.

Python:

```bash
pip install "invariant-protocol @ git+https://github.com/jim-technologies/invariantprotocol.git@v0.9.0#subdirectory=python"

# Include the optional PyArrow bridge:
pip install "invariant-protocol[data] @ git+https://github.com/jim-technologies/invariantprotocol.git@v0.9.0#subdirectory=python"
```

Rust:

```toml
[dependencies]
invariant-protocol = { git = "https://github.com/jim-technologies/invariantprotocol", tag = "v0.9.0" }

[build-dependencies]
invariant-protocol-codegen = { git = "https://github.com/jim-technologies/invariantprotocol", tag = "v0.9.0" }
```

TypeScript:

```bash
npm install --allow-git=root "github:jim-technologies/invariantprotocol#v0.9.0"
```

For reproducible production builds, replace the tag with a full commit
revision using the package manager's normal Git syntax.

Protobuf sources use the same Git revision. Pin or vendor the repository and
make its `proto/` directory an import root (a local Buf workspace/module
dependency, or a `protoc -I` path) so
`invariant/data/v1/annotations.proto` resolves without a registry.

## Development and release contract

```bash
flox activate
make generate
make check        # quality checks and coverage-gated suites for all four languages
make security
make integration  # Git-install and PostgreSQL/Atlas boundaries; requires Docker
make parity-release
make bench
```

`conformance/feature-parity.json` is the cross-language release contract. A
portable Core feature needs an idiomatic public API and behavioral test
evidence in Go, Python, Rust, and TypeScript before a repository tag can ship.
Build tools and language-specific ecosystem bridges are classified separately
instead of being duplicated for artificial symmetry. See
[feature parity](docs/feature-parity.md) and [runtime stack policy](docs/runtime-stacks.md).

CI runs quality checks, four coverage-gated suites, dependency and secret
audits, generated-code checks, clean Git installs, and PostgreSQL/Atlas
apply-inspect-diff integration. Pull requests also run protobuf breaking
checks. Dependency upgrades are intentional and review-driven; the repository
does not require a scheduled dependency job.

## Deliberate scope

- Native gRPC supports every declared cardinality. HTTP, MCP, and CLI project
  unary and server-streaming; remote proxies project unary only.
- HTTP serving is Connect-only. Invariant does not generate a first-class REST
  server from `google.api.http` annotations.
- The MCP surface is the non-SSE tool-server subset of `2025-11-25`.
- The data layer compiles and renders schemas. Deployment policy and physical
  writes remain consumer-owned, except for the explicit Python-to-PyArrow value
  bridge.

## License

Apache-2.0
