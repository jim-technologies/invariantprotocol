# Changelog

All notable changes to this project are documented here. Go, Python, Rust, and
TypeScript share the version in `VERSION` and are released together from one
repository tag named `vX.Y.Z`.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
the project is pre-1.0 so 0.x minor releases may include deliberate API changes,
but never silent wire-behavior regressions.

## Unreleased

## v0.13.0 — 2026-07-28

### Added

- **Fixed-cardinality lists are now canonical schema shape.** The aggregate
  field option accepts a positive, bounded `fixed_list` length on repeated
  `float` and `double` carriers. SchemaBundle IR v4/mapping v3 preserves that
  dimension through stable identities and rejects same-number dimension
  changes. Arrow and Python emit native FixedSizeList values; Parquet and
  Iceberg report their unenforced widening; PostgreSQL emits an exact
  top-level JSONB cardinality constraint, and ClickHouse emits exact recursive
  array-length checks.
- **LanceDB is qualified through the canonical Arrow boundary.** The locked
  LanceDB 0.34.0/PyArrow 25.0.0 integration creates, appends, reopens, indexes,
  searches, merges, and optimizes using only Invariant-generated schema and
  data. Lance files, indexes, primary keys, MemWAL/LSM, compaction, credentials,
  and namespace policy remain SDK/application configuration; Invariant does
  not implement a Lance writer or a parallel Lance schema. Qualification also
  records LanceDB's application-owned format-2.2 requirement for Arrow maps,
  fail-closed NaN vector policy, and persisted list-child metadata limitation.

### Changed

- **Historical SchemaBundles have one explicit migration path.** Go, Python,
  Rust, and TypeScript readers migrate the exact IR-v3/mapping-v2 pair to
  IR-v4/mapping-v3 without changing stable IDs or tombstones.
  `invariant-schema migrate` rewrites that artifact deterministically; unknown
  fields and every other version pair still fail closed.

## v0.12.3 — 2026-07-27

### Fixed

- **Rust cardinality conformance now uses the shared descriptor image.** The
  build-time test bindings are generated from
  `conformance/proto/descriptor.binpb`, removing a hand-built duplicate schema
  with different package and message identities.

### Documentation

- **The descriptor artifact lifecycle is explicit.** Applications may generate
  an untracked image in CI but must package or embed the exact code-generation
  input for runtime metadata. This Git-only repository commits and verifies its
  reproducible descriptor fixtures, while committed SchemaBundles remain
  durable evolution state.

## v0.12.2 — 2026-07-26

### Fixed

- **Tag CI now verifies installations from complete Git history.** Package
  managers may traverse annotated-tag ancestry, so the integration checkout is
  intentionally unshallow and release-tag verification rejects shallow
  checkouts early with an actionable diagnostic. The temporary source still
  uses an independent Git transport clone and never includes worktree state.
- **Connect interoperability readiness no longer includes compiler latency.**
  The Go and Rust fixtures are built before their servers start, so the
  readiness deadline measures process startup instead of a cold toolchain
  build.

## v0.12.1 — 2026-07-26

### Changed

- **Release-tag verification now installs the exact root tag.** CI requires the
  tag to match the repository version and checked-out commit before testing
  clean Go, Python, Rust, and TypeScript installation, and every tag still runs
  the complete release gates.
- **Existing portability claims have independent build and wire checks.** An
  official Connect-ES client now exercises the Go, Python, and Rust HTTP
  projections in JSON and protobuf modes, while imported OpenAPI output is
  compiled through Go, Python, Rust, and TypeScript from one generated
  descriptor. ClickHouse tests now round-trip every semantic type and exercise
  nested/oneof evolution.

## v0.12.0 — 2026-07-24

### Added

- **SchemaBundle now projects deterministically into ClickHouse.** The
  `go/data/clickhouse` package and `invariant-schema clickhouse` command emit
  safely quoted table-body declarations without choosing an engine or physical
  layout. Native numeric, temporal, decimal, UUID, fixed-byte, Array, Map, and
  named-Tuple mappings retain source storage names and return explicit
  compatibility diagnostics.
- **Presence and unions remain lossless in the hot schema.** Optional
  scalar-like values use `Nullable(T)`, optional/required composites use an
  explicit `(present, value)` Tuple, and oneofs use both a reserved
  discriminator containing the selected protobuf field number and per-member
  presence tuples. Generated checks require both oneof representations to
  agree and cover required presence, UTF-8 strings, closed enums, and unique
  map keys.
- **Hot-to-cold publication has a versioned structural plan.**
  `ProjectToIceberg`, `ProjectionJSON`, and the
  `invariant-schema clickhouse-iceberg` command describe value and presence
  expressions without adding ingestion or catalog behavior. UInt64/fixed64
  use checked `accurateCast` to Iceberg `decimal(20,0)` with no signed or
  floating intermediate. A pinned real-ClickHouse test verifies DDL, values,
  constraints, and the UInt64 maximum.

### Changed

- **Database CLI names are dialect-specific.** `invariant-schema postgres`
  replaces the ambiguous `sql` command now that ClickHouse is also supported.
- **The repository has one conventional build and expanded data integration
  gate.** `make build` builds all four language packages; `make integration`
  now includes the guarded ClickHouse round trip alongside Git installs and
  PostgreSQL/Atlas.

## v0.11.0 — 2026-07-24

### Added

- **Existing OpenAPI contracts can bootstrap canonical protobuf source.** The
  `invariant-openapi import` build tool performs a deterministic, one-way
  conversion from bundled OpenAPI 3.0/3.1 documents, carries supported HTTP
  bindings and representable validation annotations, rejects unsupported or
  ambiguous contract mappings, and warns about deliberately unencoded policy,
  deployment, status, or HTTP-error metadata. The generated `google.api.http`
  annotations are outbound/tooling metadata; HTTP serving remains Connect-only.

## v0.10.0 — 2026-07-22

### Added

- **One bundle now renders one complete PostgreSQL desired state.** The schema
  CLI emits every dataset in deterministic order when `sql --bundle` omits
  `--message`; single-dataset Arrow, Parquet, and Iceberg output retains an
  explicit selection requirement for multi-dataset bundles.
- **Schema evolution now retains compiler-owned name provenance.** SchemaBundle
  IR v3 carries exact active and retired storage-name sources, requires
  protobuf name/number reservations at rename and removal boundaries, and
  permanently prevents a different identity from reusing a retired column
  name in the same logical scope.
- **The zero-network composition path is explicit.** Documentation distinguishes
  direct typed application calls from optional programmatic dispatch through
  Invariant's shared validation and interceptor pipeline.

### Changed

- **Dataset selection follows descriptors rather than namespace guesses.**
  Annotation discovery includes imported annotated messages, and explicit roots
  may select any real non-map message, including well-known or dependency
  messages when that is intentional.
- **Atlas integration verifies the live database contract.** The disposable
  PostgreSQL check now covers the complete multi-table desired state, types,
  defaults, nullability, comments, and exact constraint expressions before
  requiring a zero Atlas diff.

### Fixed

- **In-process RPC semantics preserve canonical gRPC behavior.** Go normalizes
  raw cancellation/deadline errors without overwriting explicit rich statuses.
  Python rejects spoofed or wrong protobuf identities and restores same-name
  cross-pool requests and responses to the registered concrete classes for
  unary and server-streaming calls.
- **PostgreSQL integration cleanup is leak-free and collision-safe.** The test
  owns containers by returned ID and removes their anonymous volumes.

## v0.9.0 — 2026-07-22

### Added

- **One stable production contract for Go, Python, Rust, and TypeScript.** The
  machine-readable parity gate now rejects a release unless every portable Core
  capability is supported with behavioral evidence in all four runtimes and
  the shared maturity is `stable`.
- **Canonical ProtoJSON schemas across every runtime.** Tool schemas preserve
  explicit `json_name` values, decimal-string 64-bit integers, non-finite float
  spellings, proto2 optional presence, well-known type shapes, and legal map-key
  strings. Generated Python type stubs and the `py.typed` marker ship with Git
  installs and authored Python code is checked with stable mypy.

### Changed

- **Projection identities are globally unambiguous.** MCP, CLI, catalogs, and
  in-process invocation use `package.Service.Method`; CLIs require
  `package.Service Method`. Short service aliases were removed so equal service
  names in different packages can coexist deterministically.
- **All projection JSON is canonical ProtoJSON.** HTTP/Connect, MCP, CLI, and
  explicit JSON conversion at projection boundaries use descriptor JSON names
  and canonical scalar representations rather than forcing protobuf source
  names. Programmatic invocation continues to return generated messages.
- **The four runtimes advance as one pre-1.0 release.** All language packages
  continue to ship together from the single root `vX.Y.Z` Git tag and follow
  Semantic Versioning; 0.x minor releases may still deliberately refine APIs.
- **Runtime registries have explicit ownership boundaries.** Python and
  TypeScript expose immutable tool metadata and detached descriptor views while
  retaining executable generated handlers and mutable registration state
  internally. Projection adapters can no longer accept caller-fabricated tool
  records or mutate a frozen server through a catalog value.
- **Supported dependency graphs were refreshed together.** Go, Python, Rust,
  TypeScript, Protobuf-ES generation, and the reproducible toolchain lock now
  use their latest compatible stable releases; Node remains on its LTS line.

### Fixed

- **Buffered MCP HTTP streaming is bounded during collection.** Every runtime
  stops reading a server stream before the aggregate JSON-RPC response exceeds
  the server-wide unary response limit and returns `resource_exhausted`; stdio
  and direct dispatch retain their documented behavior. TypeScript also no
  longer mistakes a normally consumed request for a disconnect and leaves the
  response open.
- **Projection lifecycle leaks and CLI metadata were corrected.** Python
  releases completed stdio tasks and does not construct an HTTP client before
  registration succeeds. Go CLI help now renders required fields and enum
  choices from the generated schema representation.
- **Python projection metadata is standards-shaped without a hot-path tax.**
  `grpc.aio.Metadata` remains the cached public context value but is allocated
  lazily, avoiding three unused containers on ordinary in-process calls.
- **Programmatic Go dispatch enforces protobuf request identity.** A request
  with a different descriptor full name now fails with `invalid_argument`
  instead of being silently reinterpreted through JSON; the same protobuf type
  from an isolated descriptor pool still crosses through binary encoding.
- **Adapter boundaries now use their canonical owners.** The schema CLI uses
  the shared SchemaBundle parser and serializer, Python's HTTP client requires
  its isolated descriptor pool and no longer imports runtime types back from
  `server.py`, and TypeScript clones caller-owned descriptor bytes before
  registration or reflection can retain them.
- **TypeScript MCP-over-HTTP has one absolute deadline owner.** The monotonic
  transport deadline now drives both handler context reporting and transport
  cancellation, eliminating a sub-millisecond race that could return an MCP
  HTTP 200 response after the configured deadline.

## v0.8.3 — 2026-07-17

### Added

- **The authored annotation path is now tested end to end.** A real annotated
  dataset is discovered without `--message` and rendered as Arrow, Parquet,
  Iceberg, and PostgreSQL from one committed bundle; PostgreSQL/Atlas
  integration exercises decimal, UUID, fixed-byte, and oneof mappings.
- **Go concurrency and module metadata are maintained CI boundaries.** CI runs
  the core runtime with the race detector, requires a tidy read-only module
  graph, and requires behavioral evidence for build-tool parity declarations.

### Fixed

- **Outbound HTTP transcoding follows protobuf JSON rules.** Go now maps proto
  selectors to canonical JSON names, including explicit and nested
  `json_name` values, and preserves annotated trailing slashes. TypeScript now
  applies `google.api.http` `response_body` in the correct wrapping direction
  and resolves the response field's JSON name.
- **Python's HTTP and validation boundaries now cover their complete
  contracts.** Unary Connect responses use the request codec regardless of
  `Accept`, repeated reviewed metadata values survive, and Protovalidate checks
  every client-streaming and bidi request with rich `BadRequest` details.
- **TypeScript rejects stale generated descriptor semantics and observes
  cancellation centrally.** Registration compares the complete reachable file
  graph and resolved field semantics; unary and every streaming poll honor
  pre-cancellation and absolute deadlines. Recursive request schemas terminate
  safely in both TypeScript and Python.
- **TypeScript HTTP proxy deadlines have one deterministic owner.** A caller
  deadline now remains authoritative when it is earlier than or equal to the
  configured transport timeout, avoiding duplicate timers with different error
  representations while preserving independently configured transport limits.
- **Rust projection shutdown and recovery are deterministic.** Multi-projection
  cancellation gracefully drains HTTP, aborts and joins MCP calls, synchronous
  middleware panics become `internal`, fast MCP calls remain tracked, colliding
  staged remote tools are rejected atomically, and unary Connect responses
  follow the request codec.

### Changed

- **Projection byte-limit configuration is consistent across runtimes.** Zero
  resets server limits or inherits per-method limits; invalid negative values
  are rejected where the host integer type permits them.
- **Rust test services are generated only for primary repository builds.**
  Downstream consumers no longer generate repository-only greet and
  cardinality fixtures.
- **Compatible dependency locks were refreshed.** Go compression/columnar
  transitive dependencies, Python filelock, Rust Tokio/cfg_aliases, and
  TypeScript's Vite toolchain are at their latest compatible releases.

## v0.8.1 — 2026-07-17

### Added

- **Maintained coverage gates for every runtime.** Go, Python, Rust, and
  TypeScript now run authored-source coverage in the normal CI test matrix and
  reject regressions below measured repository floors.
- **A real PostgreSQL/Atlas compatibility boundary.** Integration renders the
  committed canonical schema bundle, applies it to disposable PostgreSQL 18.4,
  inspects the live schema, and requires Atlas to report a zero diff.
- **Focused projection and code-generation regression evidence.** Go now
  exercises sibling projection shutdown, the exact served descriptor image,
  and Connect stream header/trailer semantics. Rust codegen now exercises
  custom output/runtime paths and every native RPC cardinality. TypeScript
  status and rich-detail conversions now cover the complete canonical matrix.

### Changed

- **TypeScript quality checks use Biome.** Biome owns formatting and linting,
  while `tsc` remains the separate static type checker. Vitest and its V8
  coverage provider are pinned together.
- **Reproducible quality tooling is part of the development environment.**
  Flox now supplies Atlas, cargo-llvm-cov, matching LLVM tools, and ShellCheck;
  Python and TypeScript lock their coverage and formatting dependencies
  normally.

## v0.8.0 — 2026-07-16

### Added

- **Portable protobuf data annotations.** Messages marked with
  `(invariant.data.v1.dataset)` become dataset roots, while aggregate field
  option `51974` refines string and bytes carriers into decimal, UUID, and
  exact-width byte domains without target-specific type strings. The compiler
  rejects invalid carriers, parameters, visible option collisions, unknown
  future option fields, and incompatible evolution.
- **Native mappings for the common data targets.** Arrow, Parquet, Iceberg, and
  PostgreSQL render their native decimal, UUID, and fixed-byte equivalents.
  Python's PyArrow bridge also validates canonical record values and converts
  them without coercion.

### Changed

- **Annotated root discovery is now the convention.**
  `invariant-schema compile` discovers annotated messages when `--message` is
  omitted; explicit roots remain available for controlled builds and take
  precedence. Dataset roots stay append-only once the generated bundle is
  committed.
- **Field storage names survive same-number source renames.** The compiler
  retains committed field IDs and storage names while the dataset protobuf full
  name remains stable, preventing a source rename from silently becoming a
  physical column rename.

### Breaking

- **SchemaBundle IR and mapping versions are now 2.** Version 1 artifacts and
  readers fail clearly rather than being guessed forward. There are no external
  v1 data-schema consumers, so this release regenerates the committed baseline
  instead of carrying a compatibility layer.

## v0.7.1 — 2026-07-16

### Fixed

- **Native reflection now matches the served surface.** Every runtime keeps
  reflection available even before application registration, advertises only
  actually registered services and methods, includes served unary remote
  proxies, and avoids stale descriptor source locations after filtering.
- **MCP transport handling is strict and consistent across runtimes.**
  JSON-RPC request IDs, parameters, client responses, malformed UTF-8, content
  types, cancellation notifications, deadlines, and bounded HTTP responses now
  follow one shared observable contract. Initialize requests validate the
  required client fields and negotiate unsupported requested versions to the
  sole supported version. Protocol-cancelled stdio calls never emit a late
  response, even if application code catches cancellation.
- **HTTP projection boundaries now preserve their full contract.** Absolute
  `Connect-Timeout-Ms` deadlines cover request bodies and application work,
  long Node.js deadlines avoid timer overflow, disconnects cancel in-flight
  handlers, sensitive `-bin` metadata aliases are rejected, unsupported media
  types are deterministic, and oversized success or rich-error responses
  cannot bypass configured limits. Connect control envelopes have a separate
  bounded allowance, so tiny application-message limits still produce valid
  success or `resource_exhausted` end-stream frames.
- **Remote and typed error behavior is preserved.** Python HTTP client timeouts
  map to `deadline_exceeded`, TypeScript retains serialized protobuf rich-error
  details, and remote native proxy registration no longer disappears behind
  projection filters.
- **TypeScript native gRPC now preserves metadata and flow control.** Repeated
  ASCII and binary request metadata, response headers, and trailers survive
  local and remote-proxy calls; malformed binary metadata fails
  deterministically instead of being silently discarded. Server-streaming and
  bidi handlers now honor grpc-js writable backpressure and cancellation.
- **Rust response streams now recover panics for their full lifetime.** A
  panic after a server-streaming or bidi response has started becomes an
  `internal` status carrying the canonical method path, matching setup-time
  panic handling without changing ordinary mid-stream statuses.
- **Data compilation rejects empty normalized storage identifiers.** Valid
  protobuf source names made only from underscores can no longer produce
  unnamed datasets or fields that some storage targets accept and PostgreSQL
  rejects.
- **Dynamic protobuf JSON mappings expose their real domain.** Arrow, Parquet,
  Iceberg, PostgreSQL, and Python now report the range reduction for unresolved
  `Any` values and non-finite `Struct`/`Value` numbers; Python conversion errors
  include the canonical path and protobuf source field.
- **CLI request behavior now matches across all runtimes.** JSON, `.binpb`, and
  `.pb` files use strict protobuf decoding, malformed files preserve
  `invalid_argument`, unexpected arguments are rejected, and streamed results
  remain newline-delimited and flushed as they are produced.

### Changed

- **The reproducible Rust toolchain is now 1.96.1.** This is the newest
  coherent Rust/Cargo/Clippy/rustfmt set currently available in the Flox
  catalog and includes the Cargo and bundled libssh2 security fixes shipped in
  Rust 1.96 and 1.96.1.
- **Reviewed dependency locks were refreshed.** Rust now resolves Tokio
  1.52.4, while Python resolves filelock 3.30.0 and ty 0.0.60.
- **gRPC test bindings now use locked local generators.** Buf still resolves
  pinned module inputs, while grpcio-tools and protoc-gen-go-grpc run locally
  so generation is not coupled to hosted plugin rate limits.

## v0.7.0 — 2026-07-15

### Added

- **Generated gRPC services are the canonical application model in every
  runtime.** Go uses `grpc.ServiceRegistrar`, Python accepts the normal grpcio
  generated registration helper, Rust generates Tonic service bridges, and
  TypeScript registers generated Protobuf-ES descriptors. Native gRPC retains
  every RPC cardinality, normal client/server controls, reflection, typed
  messages, status details, metadata, deadlines, cancellation, and graceful
  shutdown without an in-memory proxy hop.
- **For locally registered services, optional projections reuse the registered
  implementation directly.** Connect HTTP, MCP, CLI, and in-process calls
  share canonical method names, typed unary/server-streaming handlers,
  validation, middleware, and bounded request/response messages. Shared
  middleware uses each language's standard gRPC or Connect abstraction where
  the host type system permits it and runs exactly once.
- **Cross-language release parity contract.**
  `conformance/feature-parity.json` records the idiomatic Go, Python, Rust, and
  TypeScript surface and behavioral test evidence for every portable feature.
  Tag builds run the strict parity gate and fail while any Core capability is
  missing in one language; maintainers run `make parity-release` before
  creating the shared root tag. Shared build tools and explicitly scoped
  ecosystem adapters remain single implementations by design.

- **Protobuf is now the authored logical data contract.** Explicit root
  messages compile into the versioned `invariant.data.v1.SchemaBundle`, which
  preserves exact scalar kinds, enum numbers and aliases, presence, declared
  defaults, comments, JSON names, nested collection shapes, and numeric source
  paths.
- **Storage-safe schema evolution.** Generated bundles retain globally unique
  field IDs across renames, allocate nested/list/map identities monotonically,
  tombstone removed IDs permanently, and reject retired-number reuse or an
  incompatible type/presence change on an active identity. Dataset root sets
  are append-only, and lossy storage-name collisions or extension-bearing
  messages fail compilation instead of producing incomplete schemas.
- **Native data-schema renderers and CLI.** `invariant-schema` emits Arrow IPC,
  an official Parquet schema, official Iceberg schema JSON, and PostgreSQL DDL
  suitable for Atlas. Every mapped node reports lossless, widening, range or
  precision reduction, representation change, or unsupported behavior.
- **One artifact across all languages.** Go owns descriptor compilation;
  Python, Rust, and TypeScript expose generated readers for the same protobuf
  bundle instead of maintaining independent inference engines. Public readers
  reject unsupported IR or mapping versions.
- **Python Arrow and Parquet value projection.** The optional `data` dependency
  maps a bundle dataset to `pyarrow.Schema` and matching generated protobuf
  messages to `pyarrow.Table`, preserving presence, oneofs, enum numbers,
  nanosecond temporal values, deterministic maps, and stable field IDs. Normal
  `pyarrow.parquet.write_table` writes the resulting table. Conversion rejects
  stale same-name message descriptors instead of allowing Arrow to coerce a
  value from a different protobuf schema.
- **One compiled service graph for codegen and runtime.** The build now creates
  `descriptor.binpb` first and passes that image to pinned Buf plugins.
  Generated registration in all four runtimes fails fast if service methods,
  cardinalities, or reachable message/enum schemas disagree with the
  descriptor used by projections. Descriptor-only proxies use isolated
  registries and do not depend on generated-module import order.
- **Python projection contexts follow grpcio.** Generated handlers receive a
  `grpc.aio.ServicerContext`-compatible object on HTTP, MCP, CLI, and in-process
  calls, including status/abort, deadlines, cancellation, transport peer
  information, completion callbacks, and initial/trailing metadata. HTTP
  request metadata is a reviewed tracing/request-ID allowlist; arbitrary
  authorization headers remain outside the gRPC metadata boundary. Projection
  peer information is not an authenticated caller identity.

### Breaking

- **Go now exposes only the canonical gRPC-native API.** Implement generated
  server interfaces and register them with `Register<Service>Server`; use
  `Serve(listener)` for native gRPC, `ConnectGRPC` for remote connections, and
  grpc-go's unary and stream interceptor types directly. `Register`, `Connect`,
  `ServeGRPC`, `GRPC`, the Invariant interceptor and stream aliases, and the
  public `Tool.Handler` field have been removed. `HTTPHandler()` now returns a
  single `http.Handler`.
- **Registration is strict.** Every runtime rejects duplicate services/tools,
  descriptor drift, wrong cardinality, and late registration deterministically
  during setup. Projection filters must be configured before the first local
  or remote registration.
- **Python local services use generated gRPC registration.** Implement generated
  servicer interfaces and call `add_<Service>Servicer_to_server`; the old
  reflection-based `Server.register()` API has been removed. Native gRPC,
  HTTP, MCP, CLI, and in-process invocation reuse the captured typed generated
  handlers and codecs. Use `connect_grpc()` with a caller-owned
  `grpc.aio.Channel`, configure and start the server returned by
  `grpc_server()`, and use `serve_projections()` for HTTP, MCP, and CLI.
  Shared middleware is one standard async `grpc.aio.ServerInterceptor`
  registered through `use()`; the old `connect()`, `serve()`, and
  `use_stream()` spellings have been removed.
- **TypeScript registration and transport ownership now follow generated
  service APIs.** Call `register(Service, implementation)`, pass a
  caller-owned grpc-js client to `connectGrpc()`, and bind the server returned
  by `grpcServer()`. Shared unary and streaming middleware uses one standard
  Connect-ES `Interceptor` through `use()`. The old descriptor-only
  `register(servicer)`, address-owning `connect()` / `serveGrpc()`, and custom
  `useStream()` APIs have been removed.
- **Connect clients accept only canonical errors.** Wrapped or uppercase error
  bodies and the non-standard `cancelled` code are no longer accepted. Wire
  errors use lowercase, unwrapped Connect envelopes and the canonical
  `canceled` spelling in every language.
- **Internal package surfaces were removed.** Python no longer re-exports
  generated descriptor message aliases, Rust no longer exports the empty
  `validation` module, and TypeScript no longer exports internal gRPC binding,
  HTTP proxy, or MCP dispatch helpers from the package root.

### Changed

- **HTTP behavior is narrower and deterministic.** TypeScript serves Connect
  only (not gRPC or gRPC-Web on the HTTP projection), all clients use Connect's
  exact malformed-body HTTP status fallback, and query parameters follow the
  protobuf/HTTP field model without a top-level `query` wrapper shim.
- **Dead code and unused dependencies were deleted** across Go, Python, Rust,
  and TypeScript, including obsolete manual Python examples and generated empty
  stubs.
- **CI no longer archives `/nix/store`.** Flox resolves its environment without
  a multi-gigabyte generic Actions cache, avoiding corrupt or partial restores
  across parallel branch and tag jobs. Jobs now declare read-only repository
  permissions and explicit timeouts.
- **Dependency upgrades are intentional and review-driven.** Scheduled
  Dependabot automation has been removed. Maintainers run `make deps`, review
  the lockfile diff, and then run the normal quality, security, and clean
  Git-install checks.
- **Go now requires 1.26.5.** The patched toolchain closes GO-2026-5856 in
  `crypto/tls`; Flox explicitly selects that exact checksum-verified Go
  toolchain while its package catalog catches up to the security release.

## v0.6.1 — 2026-07-15

### Changed

- **Git-only distribution is explicit and guarded.** Invariant-owned packages
  install from a repository tag or immutable commit rather than PyPI, the npm
  registry, crates.io, or another language registry. npm and Cargo metadata
  prohibit accidental publication, PyPI rejects the package's private
  classifier, and CI verifies clean Git consumers.

## v0.6.0 — 2026-07-14

### Changed

- **One version and one release tag.** Every language package now reports
  `0.6.0`, with the repository-root `VERSION` as the source of truth. CI checks
  package manifests, lockfiles, runtime version constants, changelog state, and
  release tags for drift. Future releases use only a plain root tag such as
  `v0.6.0`; the historical `go/v*` tags remain immutable but are retired.
- **Go is now part of the root repository module.** The module path is
  `github.com/jim-technologies/invariantprotocol`, while the package remains in
  `go/`, so source imports stay
  `github.com/jim-technologies/invariantprotocol/go`. The manual example is
  part of the same module instead of maintaining a second dependency graph.
- **TypeScript has one npm package boundary.** The publishable manifest,
  dependency lock, scripts, and development dependencies now live at the
  repository root; TypeScript source and configuration remain under
  `typescript/`.
- **Release automation follows the monorepo boundary.** Dependabot, Make
  targets, linting, tests, dependency updates, and tag-triggered CI now operate
  on the root Go and npm packages.

### Go module migration

The old module requirement was:

```text
github.com/jim-technologies/invariantprotocol/go v0.3.1
```

Starting with this release, require the repository module instead:

```text
github.com/jim-technologies/invariantprotocol v0.6.0
```

Go source imports do not change. Root-module tagging begins with `v0.6.0`.

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
  now uses Python 3.14, Go 1.26, Node 24 LTS, Rust 1.95 with edition 2024,
  GCC 16, Buf 1.71, and the latest compatible linting and generation tools
  available in the catalog.
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

- **Rust implementation** (new). Descriptor-driven dispatch via prost-reflect
  with explicit per-method registration.
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
