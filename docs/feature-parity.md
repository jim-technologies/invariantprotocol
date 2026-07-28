# Cross-language feature parity

Invariant releases one repository version for Go, Python, Rust, and TypeScript.
That version promises semantic parity for portable Core behavior, not identical
spelling or identical implementation structure.

`conformance/feature-parity.json` is the machine-readable release contract.
`make parity` validates it during development; `make parity-release` rejects a
release while any Core row is incomplete or the Core maturity is not `stable`.

## Release rule

A portable public feature requires all of the following in the same release:

1. an idiomatic API in all four runtimes;
2. the same observable RPC and wire semantics;
3. behavioral test evidence for every runtime; and
4. documentation using each ecosystem's normal terminology.

A similarly named method is not evidence. Tests must exercise the applicable
typed messages, full method identity, statuses and details, metadata, deadlines,
cancellation, limits, registration freeze, and lifecycle behavior.

## Idiomatic shape mirror

| Concept | Go | Python | Rust | TypeScript |
| --- | --- | --- | --- | --- |
| Load descriptor | `ServerFromDescriptor` | `Server.from_descriptor` | `Server::from_descriptor` | `Server.fromDescriptor` |
| Register generated service | `RegisterXServer(server, impl)` | `add_XServicer_to_server(impl, server)` | generated `register_x_server(server, impl)` | `server.register(XService, impl)` |
| Native gRPC | `Serve(listener)` | `grpc_server()` | `grpc_routes()` + Tonic host | `grpcServer()` + grpc-js host |
| Remote gRPC | `ConnectGRPC` | `connect_grpc` | `connect_grpc` | `connectGrpc` |
| Remote HTTP | `ConnectHTTP` | `connect_http` | `connect_http` | `connectHttp` |
| Shared middleware | `Use` / `UseStream` | `use` with `grpc.aio.ServerInterceptor` | `use_shared_unary` / `use_shared_stream` | `use` with Connect-ES `Interceptor` |
| In-process call | `Invoke` / `InvokeStream` | `invoke` / `invoke_stream` | `invoke` / `invoke_stream` | `invoke` / `invokeStream` |
| HTTP host adapter | `HTTPHandler` | `asgi_app` | `http_router` | `httpHandler` |
| Per-method limits | `ConfigureMethod` | `configure_method` | `configure_method` | `configureMethod` |
| Data bundle read/migrate | `data.ParseSchemaBundle` / `data.MigrateSchemaBundle` | `parse_schema_bundle` / `migrate_schema_bundle` | `parse_schema_bundle` / `migrate_schema_bundle` | `parseSchemaBundle` / `migrateSchemaBundle` |

Generated conventions win over artificial symmetry. grpcio keeps its generated
helper argument order. Rust uses generated Tonic traits and routes. TypeScript
uses generated Protobuf-ES descriptors and Connect-ES service/interceptor types.
Python and TypeScript use one standard interceptor abstraction for unary and
streaming; Go's standard API intentionally has separate unary and stream types.

## Capability classes

- **Core** means portable runtime behavior and must be supported in all four
  languages before release. `supported` is a stable, production-ready status;
  preview or experimental work cannot satisfy the release gate.
- **Build tool** means one language-neutral program produces or consumes shared
  contract artifacts for every runtime. The SchemaBundle compiler belongs here
  because four inference engines would create four competing canonical mappings.
  The one-way OpenAPI importer also belongs here: every language consumes its
  generated protobuf contract through normal Buf generation rather than
  carrying a runtime-specific importer.
- **Ecosystem** means an adapter to a language-native library. PyArrow record
  conversion and maintained Protovalidate implementations are examples; their
  narrower availability must be explicit but does not imply a server-runtime
  parity gap.

The common MCP contract is the non-SSE tool-server subset of `2025-11-25`. The
common HTTP contract is Connect on canonical full method paths, including four
independent HTTP message limits and a reviewed metadata mapper. Native gRPC
continues to use each language's normal transport controls and lifecycle.
Projection tool identities are always `package.Service.Method`, and every JSON
surface and generated input schema follows canonical ProtoJSON names and scalar
representations. The stateless `/mcp` endpoint applies the server-wide unary
response limit to its aggregate JSON-RPC document; per-method overrides remain
attached to canonical Connect routes.

When a runtime has a useful extra integration, document it as such without
quietly expanding a Core row. For example, Go, Python, and TypeScript currently
consume a primary `google.api.http` binding in their remote HTTP adapters, while
the portable remote-HTTP contract is the canonical Connect method path.

The data compiler follows the same rule: dataset and field annotations are
language-neutral protobuf options, one Go build tool compiles the descriptor
image, and all four languages decode and migrate the same versioned
SchemaBundle. Decimal, UUID, fixed-byte, and fixed-list refinements are
generated IR variants in every reader. The same build tool renders Arrow,
Parquet, Iceberg, PostgreSQL, and ClickHouse artifacts, including a
language-neutral structural ClickHouse-to-Iceberg publishing plan; duplicating
target inference in four runtimes would create competing schemas. PyArrow
record conversion and LanceDB qualification remain Python ecosystem adapters.
LanceDB consumes the canonical Arrow table; it is not a sixth renderer or a
portable server-runtime feature.

The OpenAPI importer is a build-time bootstrap, not a portable runtime surface
or synchronization engine. It accepts bundled OpenAPI 3.0/3.1 input, produces
deterministic reviewable proto3 source, and fails when an external reference or
unsupported request/success-response construct would make the canonical mapping
ambiguous or lossy. Once reviewed, that protobuf source is the sole authored
contract for Go, Python, Rust, and TypeScript. Imported `google.api.http`
annotations retain outbound HTTP intent for supported clients and tooling;
Invariant's HTTP server continues to expose only canonical Connect routes.
