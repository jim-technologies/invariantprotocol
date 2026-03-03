# Invariant Protocol

Write a gRPC service once. Get MCP tools, CLI commands, HTTP endpoints, and gRPC servers — from the same code.

## How it works

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

The proto descriptor already contains every service name, method signature, field type, enum value, and source comment. Invariant reads it and projects your services into whatever interface you need.

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

## Example

Define your service in proto:

```protobuf
syntax = "proto3";
package greet.v1;

// A greeting service.
service GreetService {
  // Greet a person by name.
  rpc Greet(GreetRequest) returns (GreetResponse);
}

message GreetRequest {
  // Name of the person to greet.
  string name = 1;
}

message GreetResponse {
  string message = 1;
}
```

Build the descriptor:

```bash
buf build --include-source-info -o descriptor.binpb
```

### Go

```go
package main

import (
    "context"
    invariant "github.com/jim-technologies/invariantprotocol/go"
    greetpb "path/to/gen/greet/v1"
)

type GreetServicer struct{}

func (s *GreetServicer) Greet(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
    return &greetpb.GreetResponse{Message: "Hello, " + req.Name}, nil
}

func main() {
    srv, _ := invariant.ServerFromDescriptor("descriptor.binpb")
    srv.Register(&GreetServicer{})

    // Pick one, or combine:
    srv.Serve(invariant.MCP())                              // MCP over stdio
    srv.Serve(invariant.CLI())                              // CLI from os.Args
    srv.Serve(invariant.HTTP(8080))                         // HTTP server
    srv.Serve(invariant.GRPC(50051))                        // gRPC server
    srv.Serve(invariant.HTTP(8080), invariant.GRPC(50051))  // multiple at once
}
```

### Python

```python
from invariant import Server

class GreetServicer:
    def Greet(self, request, context):
        return greet_pb2.GreetResponse(message=f"Hello, {request.name}")

server = Server.from_descriptor("descriptor.binpb")
server.register(GreetServicer())

# Pick one, or combine:
server.serve(mcp=True)                            # MCP over stdio
server.serve(cli=True)                            # CLI from sys.argv
server.serve(http=8080)                           # HTTP server
server.serve(grpc=50051)                          # gRPC server
server.serve(http=8080, grpc=50051)               # multiple at once
```

### Remote proxy (zero code)

Don't have the source? Point at a running gRPC server:

```go
srv, _ := invariant.ServerFromDescriptor("descriptor.binpb")
srv.Connect("localhost:50051")       // proxy all methods
srv.Serve(invariant.HTTP(9090))      // expose as HTTP
```

Your existing gRPC server is now an HTTP API. Or an MCP tool. Or a CLI.

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

## What each projection does

| Projection | What happens |
|------------|-------------|
| **MCP** | Each unary RPC becomes a tool. JSON Schema from proto types. Descriptions from proto comments. Served over stdio. |
| **CLI** | `<binary> ServiceName Method -r '{"name":"Alice"}'` or `-r request.yaml`. `--help` shows all methods with field types and descriptions. |
| **HTTP** | `POST /{package.ServiceName}/{Method}` with JSON body. ConnectRPC-compatible routing. |
| **gRPC** | Standard gRPC server. Dynamic dispatch from descriptor — no generated server stubs needed. |

## Proto comments flow everywhere

```protobuf
// Run a backtest with a given strategy and time range.
rpc RunBacktest(RunBacktestRequest) returns (RunBacktestResponse);

message RunBacktestRequest {
  // Trading symbols to test, e.g. BTCUSDT.
  repeated string symbols = 1;
}
```

These comments automatically appear in:
- MCP tool descriptions (what the LLM reads)
- CLI `--help` output (what the human reads)
- JSON Schema `description` fields

Write once, appear everywhere.

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

- Proto descriptor: `buf build --include-source-info -o descriptor.binpb`
- Generated stubs for your language (the ones you already have from `buf generate`)
- That's it

## License

Apache 2.0
