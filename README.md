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
```

Write the comment once. It appears everywhere — typed, validated, with enums, required fields, and descriptions.

## The API

```
from_descriptor("descriptor.binpb")   # load proto descriptor
from_bytes(raw_bytes)                 # load from embedded bytes

register(servicer)                    # wire your implementation
connect("host:port")                  # or proxy to a remote gRPC server

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

## Middleware

Standard gRPC `UnaryServerInterceptor` pattern. Runs on every invocation across all projections.

```go
srv.Use(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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

First registered = outermost. Existing gRPC ecosystem interceptors plug in directly (Go).

## Projections

| Projection | What happens |
|------------|-------------|
| **MCP** | Each unary RPC becomes a tool. JSON Schema from proto types. Descriptions from proto comments. Served over stdio. |
| **CLI** | `ServiceName Method -r request.yaml`. `--help` shows all methods with field types and descriptions. |
| **HTTP** | `POST /{package.ServiceName}/{Method}` with JSON body. ConnectRPC-compatible routing. |
| **gRPC** | Standard gRPC server. Dynamic dispatch from descriptor — no generated server stubs needed. |

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
- That's it

## License

Apache 2.0
