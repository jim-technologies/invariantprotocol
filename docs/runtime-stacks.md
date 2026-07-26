# Runtime stack policy

Invariant uses the smallest maintained, language-native stack that preserves
one protobuf/gRPC programming model. A dependency is not preferred merely
because it is newer or more feature-rich: it must be supported, idiomatic, and
fit the canonical generated-service architecture.

## Architecture boundary

Invariant uses a deliberately small ports-and-adapters architecture, not a
ceremonial enterprise DDD layer stack:

- authored protobuf plus generated service interfaces are the canonical service
  contract;
- registered typed dispatch is the application core and the only execution
  path for shared middleware, status, metadata, deadlines, and cancellation;
- native gRPC, HTTP/Connect, MCP, and CLI are inbound adapters over that core;
- remote gRPC and HTTP clients are outbound adapters; gRPC connections remain
  caller-owned, while HTTP transport ownership follows each language's
  idiomatic client API; and
- the schema compiler and its Arrow, Parquet, Iceberg, PostgreSQL, and
  ClickHouse renderers form a separate data-schema boundary around the shared
  `SchemaBundle` model.

`Server` is the composition root and convenience facade, so it may construct
adapters. An adapter must still re-enter the registered typed dispatch path; it
must not introduce a second handler model or a hidden transport hop. There are
no repository, aggregate, or use-case abstractions because this framework has
no application business domain for them to model.

Applications may still keep richer internal domain models behind their
generated service implementations; Invariant governs contract projection, not
the application's business-modeling strategy.

## Selected stacks

| Runtime | Canonical RPC stack | Projection host | Policy |
| --- | --- | --- | --- |
| Go | grpc-go + protobuf-go | `net/http` | Generated `Register<Service>Server` and `grpc.ServiceRegistrar` are the public model. Ordinary `grpc.ServerOption` and interceptor types remain native. |
| Python | grpcio AsyncIO + Google Protobuf | ASGI + Uvicorn, HTTPX client | Generated `add_<Service>Servicer_to_server` is the public model. The runtime is async-only; shared middleware uses `grpc.aio.ServerInterceptor`, and callers configure the returned aio server normally. |
| Rust | Tonic + Prost + Tokio/Tower | Axum, Reqwest client | Generated Tonic traits, clients, and servers are canonical. The code generator adds only the registration bridge needed to retain projection metadata; callers host extracted routes on Tonic. |
| TypeScript | grpc-js + Protobuf-ES + Connect-ES | Connect-Node + Node HTTP | A generated `DescService`, Connect-ES `ServiceImpl<T>`, and standard Connect `Interceptor` form the typed model; grpc-js remains the native gRPC transport. |

Toolchains track supported stable releases. Production runtimes use an LTS
line when the ecosystem distinguishes Current from LTS; therefore Node uses
the latest cross-platform Node 24 LTS build available in the Flox lock, not the
non-LTS Node 26 Current line. Pre-release frameworks and unsupported runtimes do
not become defaults simply to maximize version numbers. When the Flox catalog
lags a Go security patch, its Go command is only the bootstrap: the manifest's
explicit `GOTOOLCHAIN` value selects the exact checksum-verified patch release
required by `go.mod`.

## Why some protocol boundaries remain local

HTTP/Connect, MCP, and CLI must call the already registered typed service
directly. They must not require an application to implement a second service
interface, and they must not use a hidden in-memory gRPC transport.

- TypeScript uses Connect-ES because its generated Protobuf-ES service
  descriptors and handler context are the normal Node service model and can be
  shared with the grpc-js adapter.
- connect-go is stable, but its generated handler interface is separate from a
  generated grpc-go server interface. Invariant keeps its narrow Connect wire
  adapter so the application implements only the grpc-go service.
- Connect for Python is currently beta and generates a Connect-specific ASGI
  service interface. Invariant keeps the grpcio servicer canonical and adapts
  the supported Connect unary/server-streaming boundary directly.
- The official pre-1.0 Connect Rust implementation currently generates Buffa
  messages and service traits. Adopting it would replace or duplicate the
  Prost/Tonic type graph, so Rust retains a narrow Axum Connect adapter. This
  decision should be revisited if the official implementation gains a stable
  Prost/Tonic adapter.
- MCP support is deliberately the stable `2025-11-25` tool-server subset:
  initialization, ping, tool discovery/calls, notifications, stdio cancellation
  notifications, and non-SSE Streamable HTTP with cancellation and deadlines
  scoped to the current request. Stateless HTTP does not retain a cross-request
  cancellation registry. A second SDK-owned tool registry would duplicate
  Invariant's dispatcher. If resources, prompts, tasks, sampling, or SSE are
  added, re-evaluate the official MCP SDKs before expanding the local protocol
  boundary.

These local boundaries are protocol adapters, not alternate application
frameworks. Their behavioral tests cover method identity, locally generated
message types, status/details, metadata, deadlines, cancellation, limits, and
current wire requirements. Descriptor-only remote adapters may use dynamic
protobuf messages at their boundary.

`make connect-interop` complements those runtime tests with one black-box
official-client check. Connect-ES calls the Go, Python, and Rust HTTP
projections over ephemeral HTTP/1.1 listeners using both JSON and binary
encoding, covering unary success, server streaming, and a canonical error.

## Dependency rules

1. Prefer the official project and public stable APIs. Depending on exports
   marked internal or outside semantic-versioning guarantees is not allowed.
2. Keep caller ownership of listeners, credentials, client connections, and
   transport policy wherever the native framework normally does.
3. Add a dependency only when it removes meaningful protocol or security risk;
   do not add a framework to replace a small standard-library adapter.
4. Lock every development graph and verify it in CI. Registry signatures or
   ecosystem checksums are validated where the package manager supports them.
5. Run secret scanning and vulnerability audits independently from unit tests.
6. Upgrade intentionally with `make deps`, review generated lockfile changes,
   then run `make check`, `make security`, and `make integration`.

The cross-language semantic release gate is documented in
[feature parity](feature-parity.md).
