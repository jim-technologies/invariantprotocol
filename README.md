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
schema for Arrow, Parquet, Iceberg, PostgreSQL, and ClickHouse. Lance and
LanceDB consume the same canonical Arrow schema and table; Invariant does not
implement a competing Lance file writer. Explicit `--message` selection
remains available for controlled builds.

```text
.proto (the only authored contract)
  └─ buf build → descriptor.binpb
       ├─ buf generate → standard protobuf/gRPC bindings → application service
       │                                                    │
       ├─ invariant.Server ←──── generated registration ────┘
       │    └─ native gRPC / HTTP-Connect / MCP / CLI
       └─ invariant-schema → SchemaBundle
                              ├─ Arrow ──→ Lance/LanceDB SDK
                              └─ Parquet / Iceberg / PostgreSQL / ClickHouse
```

Proto comments become MCP tool descriptions, CLI help, JSON Schema field
descriptions, and constrained enum choices. Transport and interceptor method
paths use `/{package.Service}/{Method}`; tool catalogs, CLI resolution, and
in-process invocation use the collision-free `package.Service.Method` identity.
Every JSON projection emits canonical ProtoJSON field names and scalar forms,
including explicit `json_name` overrides and decimal strings for 64-bit
integers.

## Canonical change data capture

Invariant defines a transport-neutral CDC wire contract as
`io.cloudevents.v1.CloudEvent<invariant.cdc.v1.ChangeRecord>`. The stable
CloudEvents protobuf envelope carries a typed `ChangeRecord` in
`google.protobuf.Any`, preserving exact keys, record images, value domains,
source positions, transaction order, timestamps, and isolated source metadata.
The Debezium compatibility profile is semantically lossless without making a
connector, database, broker, outbox, or checkpoint runtime part of the core.
The official CloudEvents protobuf schema is vendored for reproducible
generation; its public protobuf name and wire layout are unchanged, while
language import paths remain repository-owned distribution metadata.

CDC v2 keeps that envelope and adds an explicit choice between
`FullChange` and `DeltaChange`. Full changes carry complete row images. Delta
changes carry typed, outcome-based field transitions that distinguish absent
from explicitly null values and can be inverted when the prior record state is
available, without making JSON Patch, a connector, or a storage engine part of
the core contract.
Creates and snapshot reads remain complete anchors; updates use exact field
transitions; deletes, truncates, and source messages retain their operation
semantics. Lists and maps are replaced atomically when their containing field
changes, keeping replay deterministic and the path model small.

See the [CDC contract and Debezium mapping](docs/cdc.md) and the
[CloudEvents](docs/adr/0001-cloudevents-change-record.md) and
[full/delta representation](docs/adr/0002-full-and-delta-cdc.md) architecture
decisions.

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

### Artifact lifecycle

Application repositories author contract semantics only in `.proto` files.
They may build `descriptor.binpb` in CI instead of committing it, but the exact
image used for code generation must still be embedded in, or copied beside, the
deployed application because Invariant uses its source comments and descriptors
for discovery, reflection, validation, and optional projections. Every runtime
accepts either a descriptor path or descriptor bytes.

This Git-only framework repository deliberately commits its reproducible
descriptor fixtures and generated release artifacts. Building the Invariant
framework packages from a Git revision therefore does not itself require Buf or
every language generator, and `make verify-generate` rejects drift from the
authored protobuf. These files are reviewed build artifacts, never an
independent contract.

`schema.binpb` has a different lifecycle: it is committed evolution state with
stable storage identities and tombstones. Regenerate and review it, but never
discard or hand-edit it as though it were a temporary descriptor image.

### Importing an existing OpenAPI contract

`invariant-openapi` provides a strict, one-way bootstrap for teams starting
from an OpenAPI 3.0 or 3.1 document:

```bash
go run ./go/cmd/invariant-openapi import \
  --input openapi.yaml \
  --package library.v1 \
  --go-package example.com/acme/project/gen/library/v1 \
  --output proto/library/v1/library.proto
```

The protobuf package and generated Go import path are required because OpenAPI
contains neither stable identity. The service name defaults from `info.title`;
use `--service` only when that name is not suitable. The importer accepts a
self-contained document with internal schema, parameter, request-body, and
response component references. It translates GET, POST, PUT, PATCH, and DELETE
operations with JSON bodies, path and query parameters, common schema shapes,
and supported validation constraints. It requires exactly one explicit
successful response and, where content exists, exactly `application/json`.
External references, header and cookie parameters, alternate media contracts,
callbacks, webhooks, schema composition, and ambiguous request or success
response mappings fail with a source-oriented diagnostic instead of silently
inventing a protobuf contract.

The output is deterministic, reviewable proto3 source, including comments,
field presence and validation annotations where representable, a conventional
`go_package`, and `google.api.http` bindings. Review the result once, run the
normal Buf build and generation flow for Go, Python, Rust, and TypeScript, then
maintain protobuf as the only canonical contract. This is not an OpenAPI
synchronizer or a runtime conversion path.

Generated contracts import Google API annotations and, when constraints are
present, Protovalidate annotations. Add `buf.build/googleapis/googleapis` and
`buf.build/bufbuild/protovalidate` to the consuming Buf module; the complete
fixture is in [`testdata/openapi`](testdata/openapi). The exact supported
intersection and fail-closed rules are documented in
[`docs/openapi-import.md`](docs/openapi-import.md).

Imported `google.api.http` bindings preserve the original route for
annotation-aware remote-HTTP adapters and ecosystem tooling. They do not add
REST routes to Invariant's server: HTTP serving remains Connect-only on
`/{package.Service}/{Method}`. OpenAPI security requirements do not become
trusted protobuf fields or gRPC metadata; authentication remains host-owned
policy.

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

## In-process composition

When caller and service are co-located, keep the generated service behind a
small typed application interface and call the implementation directly. That
is the normal zero-network path: it preserves generated request/response types,
context cancellation, and testability without pretending a local call has
transport metadata.

If a local caller specifically needs Invariant's shared validation and
interceptor pipeline, every runtime also exposes programmatic unary and
server-streaming invocation (`Invoke`/`InvokeStream` in Go and their idiomatic
language equivalents). Those calls use the same registered implementation and
perform no socket or hidden in-memory gRPC hop.

When the component later moves out of process, implement the same typed
application interface with the generated gRPC client. This keeps the
local-versus-remote decision at the application boundary; Invariant does not
introduce a service-locator or transparent-network abstraction.

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
  repeated float embedding = 4 [(invariant.data.v1.field) = {
    fixed_list: { length: 1536 }
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
go run ./go/cmd/invariant-schema postgres \
  --bundle ledger.schema.binpb --output ledger.sql
go run ./go/cmd/invariant-schema clickhouse \
  --bundle ledger.schema.binpb --output ledger.clickhouse.sql
go run ./go/cmd/invariant-schema clickhouse-iceberg \
  --bundle ledger.schema.binpb --output ledger.clickhouse-iceberg.json
```

`compile` reads an existing output bundle before replacing it. Numeric
protobuf paths retain active field identities, removed identities remain
tombstoned, and field storage names survive same-number source renames when the
old protobuf name remains reserved. Generated bundle names are compiler-owned;
their exact protobuf source follows active fields into permanent tombstones,
the dataset full name remains stable, and incompatible identity or storage-name
reuse fails.
Repeated `--message` flags can explicitly select roots for controlled or
one-off builds; annotation discovery is the normal convention. Commit and
review the bundle, but continue to author only protobuf.

SchemaBundle IR v4 and mapping v3 add fixed-cardinality lists. All four
language readers migrate the exact historical v3/mapping-v2 pair without
changing identities or tombstones. The build-time CLI can rewrite that
migration deterministically for review:

```bash
go run ./go/cmd/invariant-schema migrate \
  --bundle ledger.schema.binpb \
  --output ledger.schema.binpb
```

When a bundle contains multiple datasets, `postgres` emits every table in
deterministic source-message order so the result is one complete Atlas desired
state. Pass `--message` to render one table as a controlled override. Arrow,
Parquet, Iceberg, and ClickHouse artifacts each describe one dataset and
therefore require `--message` for a multi-dataset bundle.

Portable field refinements map canonical decimal text, canonical UUID text,
exact-width bytes, and fixed-cardinality float or double lists to target
types. A fixed list has a positive length of at most 2,147,483,647, is valid
only on a non-map repeated `float` or `double`, and is part of the field's
logical shape. Changing its length under the same protobuf number fails
evolution validation; add a new field number and reserve the retired one.
Python's `arrow_table()` requires exactly that many values, including rejecting
an omitted or empty vector, before constructing a native
`pyarrow.FixedSizeListArray`. Other writers must perform the same boundary
validation. The options do not encode keys, indexes, partitions, placement,
or migration policy.

The PostgreSQL renderer emits desired-state DDL directly for Atlas; HCL is not
another source format. Atlas consumes the generated `file://` SQL desired state
using a PostgreSQL development database. CI applies that SQL to disposable
PostgreSQL 18.4, verifies defaults, nullability, collection defaults, comments,
and constraints in the live catalog, and requires a zero diff. Invariant does
not infer keys, indexes, partitions, catalog operations, file layout, or
migrations, and it does not apply production database changes.

The ClickHouse renderer emits only a parenthesized table-body fragment:
deterministic columns, comments, defaults, and logical `CHECK` constraints. It
does not choose `MergeTree`, `ORDER BY`, partitions, TTLs, codecs, indexes,
projections, or storage policy. Optional scalar-like values use `Nullable(T)`;
optional composite values use `Tuple(present Bool, value T)` because stable
ClickHouse cannot wrap arrays/maps and does not enable nullable tuples by
default. Oneofs use an explicit
`__invariant_oneof_<oneof>_case Int32` discriminator whose nonzero value is
the selected protobuf field number. Each member is also a
`Tuple(present Bool, value T)`, and generated checks require the discriminator
and member presence to agree. This redundancy prevents a synthetic column from
becoming the sole source of protobuf presence and preserves selected defaults
through schema evolution. The `__invariant_` namespace is renderer-owned. A
structural ClickHouse-to-Iceberg publishing plan keeps presence separate from
values and uses `accurateCast(value, 'Decimal(20, 0)')` for the exact
full-domain `UInt64` conversion. It is not an ingestion runtime or a
ClickHouse/Iceberg catalog integration. Fixed lists remain `Array(T)` with an
exact `length(...)` check; the publishing plan carries the declared length.

Python's optional data adapter converts a bundle dataset and matching generated
messages into a real `pyarrow.Table`:

```python
import pyarrow.parquet as pq
from invariant import arrow_record_batch_reader, arrow_table, find_dataset, parse_schema_bundle

bundle = parse_schema_bundle(open("ledger.schema.binpb", "rb").read())
dataset = find_dataset(bundle, "ledger.v1.LedgerEvent")
if dataset is None:
    raise ValueError("dataset is not present in the bundle")
table, diagnostics = arrow_table(dataset, events)
pq.write_table(table, "ledger.parquet")
```

For an input that should not be materialized as one table, use the standard
single-pass Arrow reader. It converts no message until a batch is pulled and
converts at most `batch_size` messages into one batch at a time. Storage owned
by the input iterable remains caller-managed:

```python
reader, diagnostics = arrow_record_batch_reader(dataset, events, batch_size=256)
with pq.ParquetWriter("ledger.parquet", reader.schema) as writer:
    for batch in reader:
        writer.write_batch(batch)
```

This is a row bound, not a byte bound: one protobuf message can still contain a
large string, byte sequence, list, or map. The 256-row default is deliberately
conservative for embedding workloads. Mapping diagnostics are available when
the reader is created; descriptor and value errors occur when the affected
batch is pulled, so a streaming sink may already have accepted earlier valid
batches. Incremental Parquet and Arrow IPC retain these physical batch
boundaries, so narrow schemas may use a larger `batch_size` when row-group or
framing efficiency matters.

`arrow_table()` checks that the generated message descriptor still carries the
exact refinements recorded in SchemaBundle, then constructs nested Arrow arrays
explicitly so canonical UUID and JSON extension values work inside structs,
lists, and maps. Populated `Any` values resolve through that generated
descriptor's own pool; no global import order or application-side cast is
required.

The table or a fresh record-batch reader is the LanceDB boundary; no
application-side vector cast or second schema is required:

```python
import lancedb

reader, diagnostics = arrow_record_batch_reader(dataset, events)
database = await lancedb.connect_async("./data/lance")
lance_table = await database.create_table("ledger_events", schema=reader.schema)
await lance_table.add(reader)
```

Pass the complete reader to one `add()` call; do not turn each Arrow batch into
a separate insert. For a finite input that comfortably fits memory,
`arrow_table()` remains the replayable, throughput-oriented choice.

PyArrow owns Parquet file writing, while the Lance SDK owns Lance manifests,
fragments, data files, indexes, compaction, and object-store access. See
[the data-schema contract](docs/data-schemas.md) for mappings, diagnostics,
evolution rules, the qualified LanceDB lifecycle, and target limitations.

## Install

Invariant-owned packages are distributed only from Git. They are not published
to PyPI, the npm registry, crates.io, or another language registry. Every
language package and the Rust codegen crate share `VERSION` and the single root
tag `v0.16.2`; new releases do not create language-prefixed tags. The project
follows Semantic Versioning; while it remains below 1.0, minor releases may
refine the public API without weakening documented wire guarantees.

Go:

```bash
go get github.com/jim-technologies/invariantprotocol/go@v0.16.2
```

The repository is one Go module. `/go` is the package directory, so consumers
import `github.com/jim-technologies/invariantprotocol/go` while `go.mod`
records the root module revision.

Python:

```bash
pip install "invariant-protocol @ git+https://github.com/jim-technologies/invariantprotocol.git@v0.16.2#subdirectory=python"

# Include the optional PyArrow bridge:
pip install "invariant-protocol[data] @ git+https://github.com/jim-technologies/invariantprotocol.git@v0.16.2#subdirectory=python"
```

Rust:

```toml
[dependencies]
invariant-protocol = { git = "https://github.com/jim-technologies/invariantprotocol", tag = "v0.16.2" }

[build-dependencies]
invariant-protocol-codegen = { git = "https://github.com/jim-technologies/invariantprotocol", tag = "v0.16.2" }
```

TypeScript:

```bash
npm install --allow-git=root "github:jim-technologies/invariantprotocol#v0.16.2"
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
make build
make validate     # the full gate: static checks, generated-code staleness, coverage-gated suites, Go race detector
make security
make integration  # Includes local LanceDB plus Docker-backed PostgreSQL/Atlas and ClickHouse
make parity-release
make bench
```

`conformance/feature-parity.json` is the cross-language release contract. A
portable Core feature needs an idiomatic public API and behavioral test
evidence in Go, Python, Rust, and TypeScript before a repository tag can ship.
Build tools and language-specific ecosystem bridges are classified separately
instead of being duplicated for artificial symmetry. See
[feature parity](docs/feature-parity.md) and [runtime stack policy](docs/runtime-stacks.md).

Release commits are pushed to `main` and must complete the CI gate before
their single annotated `vX.Y.Z` tag is created. Tag pushes rerun the same
gate; the audit workflow verifies clean Git installs and can be dispatched
on demand.

The verb grammar follows [MAKEFILE-CONTRACT.md](MAKEFILE-CONTRACT.md):
`make validate` is the single gate, and CI runs exactly
`flox activate -- make validate` — every toolchain comes from the Flox
manifest, so CI cannot drift from the local gate.
`make release` checks release readiness from the root `VERSION` and refuses a
dirty or unpushed tree; it never publishes to language registries, because
Invariant packages are deliberately Git-distributed only.

Network-dependent verification lives in the secretless weekly `audit`
workflow (also runnable on demand): dependency and secret audits, clean Git
installs, official-client Connect interoperability, a local LanceDB
lifecycle, PostgreSQL/Atlas apply-inspect-diff integration, and a real
ClickHouse DDL/value round trip. Protobuf breaking checks against
`origin/main` run inside `make validate`; `make breaking` runs that slice
alone. Dependency upgrades are
intentional and review-driven; the repository does not require a scheduled
dependency job.

## Deliberate scope

- Native gRPC supports every declared cardinality. HTTP, MCP, and CLI project
  unary and server-streaming; remote proxies project unary only.
- HTTP serving is Connect-only. Invariant does not generate a first-class REST
  server from `google.api.http` annotations. The OpenAPI importer emits those
  annotations as outbound/tooling metadata; it does not change serving
  behavior.
- The MCP surface is the non-SSE tool-server subset of `2025-11-25`.
- The data layer compiles and renders schemas. Deployment policy and physical
  writes remain consumer-owned, except for the explicit Python-to-PyArrow value
  bridge. Lance/LanceDB is qualified through that Arrow boundary; Invariant
  does not own Lance files, indexes, primary keys, MemWAL/LSM policy,
  compaction, credentials, or manifest placement.

## License

Apache-2.0
