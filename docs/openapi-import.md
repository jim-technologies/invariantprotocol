# OpenAPI import

`invariant-openapi` is a build-time migration tool for an existing OpenAPI
contract. It produces deterministic proto3 source for review. After that review,
the `.proto` file is the canonical contract; the importer is not a bidirectional
synchronizer and does not run in an application process.

## Usage

```bash
go run ./go/cmd/invariant-openapi import \
  --input openapi.yaml \
  --package library.v1 \
  --go-package example.com/acme/project/gen/library/v1 \
  --output proto/library/v1/library.proto
```

`--package` and `--go-package` are required because OpenAPI has neither a stable
protobuf namespace nor a Go repository import path. `--go-package` is the import
path only; package aliases are intentionally not accepted. The service name
comes from `info.title`; `--service` provides the only naming override. Input is
limited to 16 MiB and must be a bundled OpenAPI 3.0.x or 3.1.x document.
Internal schema, parameter, request-body, and response component references are
supported. Filesystem and network references are never loaded.

Generated source imports Google API annotations and, when needed,
Protovalidate. A consuming `buf.yaml` therefore normally includes:

```yaml
deps:
  - buf.build/bufbuild/protovalidate
  - buf.build/googleapis/googleapis
```

Run `buf format`, `buf lint`, `buf build`, and the normal language generators
before accepting the generated source.

## Canonical mapping

- One OpenAPI document becomes one protobuf service.
- `operationId` becomes the RPC name. When it is absent, the HTTP method and
  path produce a stable name such as `GetV1BooksByBookId`.
- GET, POST, PUT, PATCH, and DELETE are supported.
- Path parameters occupy complete path segments and appear first in their path
  order. A JSON body follows, then query parameters in wire-name order.
- The importer rewrites path placeholders to protobuf field names and retains
  exact JSON/query spelling with `json_name`.
- Non-empty RPC inputs and outputs receive conventional, unique
  `MethodRequest` and `MethodResponse` messages. Shared OpenAPI object
  components remain shared messages inside those method-specific envelopes;
  scalar, array, and map component aliases are inlined.
- Exactly one explicit 2xx response is required. A response without content
  uses `google.protobuf.Empty`; another response shape uses `response_body` so
  annotation-aware outbound clients and tooling see the original unwrapped
  JSON body. The numeric success status itself is not part of the gRPC contract;
  non-200 statuses produce a review warning.
- Source descriptions become protobuf comments. Stable fallback comments are
  generated when descriptions are absent.

Message properties are sorted by original wire name before field numbers are
assigned. Methods, messages, imports, enum constraints, and warnings are also
ordered deterministically. Reordering YAML maps or converting the input to JSON
does not change the output.

## Supported schema intersection

| OpenAPI schema | Protobuf |
| --- | --- |
| `boolean` | `bool` |
| `integer`, `format: int32` | `int32` |
| `number`, `format: float` | `float` plus finite validation |
| `number`, `format: double` | `double` plus finite validation |
| `string` | `string` |
| `string`, `format: byte` | `bytes` |
| `string`, `format: date-time` | `google.protobuf.Timestamp` |
| Array of a supported non-array value | `repeated` |
| Object with `additionalProperties: false` | Generated message |
| Object containing only typed `additionalProperties` | `map<string, T>` |
| String enum | `string` with a Protovalidate `in` rule |

Representable length, maximum item/property count, scalar uniqueness, pattern,
format, and numeric-bound constraints become Protovalidate rules. Required
scalar and message fields use explicit protobuf presence, Protovalidate
presence, and `google.api.field_behavior = REQUIRED`. These annotations are
portable in the generated contract. Invariant's validation interceptors enforce
request messages where the language runtime supports Protovalidate; consult the
feature-parity matrix for current runtime availability. Handler responses are
not automatically validated.

Some apparently similar mappings are intentionally rejected:

- OpenAPI `int64` is a JSON number, while ProtoJSON encodes protobuf 64-bit
  integers as decimal strings.
- A protobuf enum would change arbitrary OpenAPI string enum values, so those
  values stay constrained strings.
- Required arrays and maps cannot retain the distinction between an absent
  property and an empty collection.
- Positive `minItems` and `minProperties` have the same presence ambiguity and
  are rejected; maximum bounds remain representable.
- OpenAPI `number` must declare `float` or `double` so precision is an explicit
  reviewed choice. Generated rules reject protobuf NaN and infinities.
- An OpenAPI object is open by default. It must be explicitly closed or be a
  pure typed map so undeclared JSON properties are not silently discarded.

ProtoJSON accepts JSON `null` as an unset field even where an OpenAPI schema
does not. This is an unavoidable semantic difference in an imported bootstrap
contract and one reason the generated source must be reviewed before protobuf
becomes canonical.

## Fail-closed boundaries

The importer reports a source-oriented error for external or unresolved
references, schema composition and unions, nullable values, defaults,
read/write-specific fields, open or mixed map objects, tuple or nested arrays,
unsupported formats and constraints, custom JSON Schema dialects, header or
cookie parameters, nonstandard parameter serialization, non-JSON media types,
ambiguous success responses, callbacks, webhooks, and unsupported HTTP methods.

Non-2xx response schemas are not turned into response messages. The importer
warns when they carry a contract; model those failures with canonical gRPC
status codes and rich error details. Security requirements also produce a
warning rather than trusted request fields or metadata. Authentication remains
an explicit responsibility of the host and its reviewed metadata policy.
Document, path, and operation server URLs are deployment configuration and
produce a warning rather than becoming part of the protobuf contract.

Finally, generated `google.api.http` annotations preserve outbound REST intent
for annotation-aware clients and ecosystem tooling. Invariant's HTTP server
remains Connect-only on the canonical `/{package.Service}/{Method}` route.
