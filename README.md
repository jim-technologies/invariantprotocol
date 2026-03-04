# Invariant Protocol

Comment your protobuf. Get AI-ready tools for free.

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

```
.proto                    descriptor.binpb                your code
  │                            │                             │
  ├── buf generate → stubs     │                             │
  └── buf build ──────────────→│                             │
       --include-source-info   │                             │
                               └── invariant runtime ←───────┘
                                        │
                          ┌──────┬──────┼──────┐
                          MCP    CLI   HTTP   gRPC
```

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
POST /v1/users
{"display_name": "Alice Smith", "email": "alice@example.com", "role": "ROLE_EDITOR", "tags": ["engineering"]}
```

Write the comment once. It appears everywhere — typed, validated, with enums, required fields, and descriptions.

## The API

```
from_descriptor("descriptor.binpb")   # load proto descriptor
from_bytes(raw_bytes)                 # load from embedded bytes

register(servicer)                    # wire your implementation
connect("host:port")                  # or proxy to a remote gRPC server
connect_http("https://api.example")   # or proxy to a remote HTTP service

use(interceptor)                      # add middleware (logging, auth, tracing)

serve(...)                            # start projections (blocking)
stop()                                # cleanup
```

Seven methods. Everything else is convention.

## Go

```go
srv, _ := invariant.ServerFromDescriptor("descriptor.binpb")
srv.Register(&GreetServicer{})

srv.Serve(invariant.MCP())                              // MCP over stdio
srv.Serve(invariant.CLI())                              // CLI from os.Args
srv.Serve(invariant.HTTP(8080))                         // HTTP server
srv.Serve(invariant.GRPC(50051))                        // gRPC server
srv.Serve(invariant.HTTP(8080), invariant.GRPC(50051))  // multiple at once
```

## Python

```python
server = Server.from_descriptor("descriptor.binpb")
server.register(GreetServicer())

server.serve(mcp=True)                            # MCP over stdio
server.serve(cli=True)                            # CLI from sys.argv
server.serve(http=8080)                           # HTTP server
server.serve(grpc=50051)                          # gRPC server
server.serve(http=8080, grpc=50051)               # multiple at once
```

## Remote proxy (zero implementation)

Don't have the source? Point at a running gRPC server:

```go
srv, _ := invariant.ServerFromDescriptor("descriptor.binpb")
srv.Connect("localhost:50051")
srv.Serve(invariant.MCP())  // your gRPC server is now an MCP tool
```

Remote HTTP service:

```go
srv, _ := invariant.ServerFromDescriptor("descriptor.binpb")
srv.ConnectHTTP("https://api.example.com")
srv.Serve(invariant.MCP()) // your HTTP service is now an MCP tool
```

```python
server = Server.from_descriptor("descriptor.binpb")
server.connect_http("https://api.example.com")
server.serve(mcp=True)  # your HTTP service is now an MCP tool
```

## HTTP Transcoding (`google.api.http`)

Invariant always exposes the canonical RPC route:

```text
POST /{package.ServiceName}/{Method}
```

If you add `google.api.http` annotations, those routes are exposed too (including `additional_bindings`):

```protobuf
import "google/api/annotations.proto";

service UserService {
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse) {
    option (google.api.http) = {
      post: "/v1/users"
      body: "*"
      additional_bindings {
        post: "/v1/create-user"
        body: "*"
      }
    };
  }
}
```

To use `google/api/annotations.proto`, add the dependency in your `buf.yaml`:

```yaml
version: v2
modules:
  - path: .
deps:
  - buf.build/googleapis/googleapis
```

Then run:

```bash
buf dep update
```

The same annotation metadata is also used by `ConnectHTTP`/`connect_http` when acting as a remote HTTP client proxy.

## Supported `google.api.http` Subset

Supported now:

- HTTP methods: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, and `custom` method kinds.
- Path templates: literal segments, `{field}`, `{field=*}`, `{field=**}` (only when `**` is the final segment).
- Body mapping: `body: "*"`, `body: "field_path"`, or omitted body.
- Canonical fallback route always available: `POST /{package.ServiceName}/{Method}`.
- Strict request decoding (unknown fields rejected with gRPC-style `INVALID_ARGUMENT` errors).

Server-side routing:

- Uses primary `google.api.http` binding and all `additional_bindings`.

Remote HTTP client proxy (`ConnectHTTP` / `connect_http`):

- Uses the primary `google.api.http` binding when present.
- Falls back to canonical RPC route if no annotation exists.
- Injects outbound HTTP headers from environment variables using:
  - `INVARIANT_HTTP_HEADER_<NAME>=value`
  - Example: `INVARIANT_HTTP_HEADER_AUTHORIZATION="Bearer <token>"`
  - Example: `INVARIANT_HTTP_HEADER_X_POLYMARKET_SIGNATURE="..."`

Not yet implemented:

- `response_body` mapping.
- Full path-template grammar beyond the patterns above.
- Client-side selection among `additional_bindings`.

## Middleware

gRPC-style unary interceptor pattern. Runs on every invocation across all projections.

```go
srv.Use(func(ctx context.Context, req any, info *invariant.ServerCallInfo, handler invariant.UnaryHandler) (any, error) {
    log.Printf("→ %s", info.FullMethod)
    resp, err := handler(ctx, req)
    log.Printf("← %s err=%v", info.FullMethod, err)
    return resp, err
})
```

```python
def logging_interceptor(request, context, info, handler):
    print(f"→ {info.full_method}")
    response = handler(request, context)
    print(f"← {info.full_method}")
    return response

server.use(logging_interceptor)
```

First registered = outermost. Existing gRPC interceptors are usually a small adapter away in Go.

## Projections

| Projection | What happens |
|------------|-------------|
| **MCP** | Each unary RPC becomes a tool. JSON Schema from proto types. Descriptions from proto comments. Served over stdio. |
| **CLI** | `ServiceName Method -r request.yaml`. AI agents that prefer shell over MCP can call it directly. Humans get `--help` with field types and descriptions. |
| **HTTP** | Canonical RPC route: `POST /{package.ServiceName}/{Method}`. Also supports `google.api.http` routes and `additional_bindings` when present. |
| **gRPC** | Standard gRPC server. Dynamic dispatch from descriptor — no generated server stubs needed. |

## Validation And Errors

- Parsing is strict by default across MCP/CLI/HTTP (unknown fields are rejected).
- Errors are gRPC-code aligned (`INVALID_ARGUMENT`, `NOT_FOUND`, etc.).
- HTTP maps gRPC status codes to HTTP status codes and returns:

```json
{
  "error": {
    "code": "INVALID_ARGUMENT",
    "message": "proto: (line 1:27): unknown field \"extra\"",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.BadRequest",
        "fieldViolations": [
          {"field": "extra", "description": "proto: (line 1:27): unknown field \"extra\""}
        ]
      }
    ]
  }
}
```

## Install

**Go:**
```bash
go get github.com/jim-technologies/invariantprotocol/go
```

**Python:**
```bash
pip install "invariant-protocol @ git+https://github.com/jim-technologies/invariantprotocol.git#subdirectory=python"
```

## Requirements

- `buf build --include-source-info -o descriptor.binpb`
- Generated stubs for your language (`buf generate`)
- If you use `google.api.http`, add Buf dep `buf.build/googleapis/googleapis` and run `buf dep update`
- No vendored `google/api/*.proto` files required

## License

Apache 2.0
