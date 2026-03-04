"""Invariant Protocol server — register gRPC servicers, project into MCP/CLI/HTTP/gRPC."""

from __future__ import annotations

import sys
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

import grpc
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

from invariant.descriptor import ParsedDescriptor
from invariant.schema import SchemaGenerator

# --- Interceptor types (mirrors gRPC pattern, zero coupling to grpc package) ---

#: Handler is the function called at the end of the interceptor chain.
#: Equivalent to Go's UnaryHandler.
Handler = Callable[[Any, Any], Any]  # (request, context) -> response

#: Interceptor intercepts unary RPCs across all projections (MCP, HTTP, gRPC, CLI).
#: Same shape as grpc.UnaryServerInterceptor but framework-native.
#: Equivalent to Go's UnaryServerInterceptor.
Interceptor = Callable[[Any, Any, "ServerCallInfo", Handler], Any]


@dataclass
class ServerCallInfo:
    """Metadata about the RPC being invoked, passed to interceptors.
    Equivalent to Go's ServerCallInfo."""

    full_method: str  # e.g. "/greet.v1.GreetService/Greet"


class Tool:
    """A single registered RPC method projected as a tool."""

    __slots__ = (
        "description",
        "handler",
        "input_schema",
        "input_type",
        "method_name",
        "name",
        "output_type",
        "service_full_name",
    )

    def __init__(
        self,
        name: str,
        description: str,
        input_schema: dict,
        handler: Callable,
        input_type: str,
        output_type: str,
        service_full_name: str,
        method_name: str,
    ):
        self.name = name
        self.description = description
        self.input_schema = input_schema
        self.handler = handler
        self.input_type = input_type
        self.output_type = output_type
        self.service_full_name = service_full_name
        self.method_name = method_name


class Server:
    """Holds parsed descriptors and registered tools, projects into MCP/CLI/HTTP/gRPC."""

    def __init__(
        self,
        parsed: ParsedDescriptor,
        *,
        name: str = "invariant-protocol",
        version: str = "0.1.0",
        fds: descriptor_pb2.FileDescriptorSet | None = None,
    ):
        self.parsed = parsed
        self.schema_gen = SchemaGenerator(parsed)
        self.tools: dict[str, Tool] = {}
        self.name = name
        self.version = version
        self._fds = fds
        self._grpc_server = None
        self._http_server = None
        self._channels: list[grpc.Channel] = []
        self._interceptors: list[Interceptor] = []

    def use(self, interceptor: Interceptor) -> None:
        """Register an interceptor. Interceptors run in registration order
        (first registered = outermost) on every tool invocation across all projections.

        Equivalent to Go's Server.Use().
        """
        self._interceptors.append(interceptor)

    def _invoke(self, tool: Tool, request: Any, context: Any) -> Any:
        """Core proto-in/proto-out dispatch — the equivalent of Go's invoke().
        Runs the interceptor chain then calls tool.handler.

        Each projection converts at its boundary:
          - MCP, HTTP: JSON → proto → _invoke → proto → JSON
          - CLI:       input → proto → _invoke → proto (JSON at terminal edge)
          - gRPC:      bytes → proto → _invoke → proto → bytes
        """
        info = ServerCallInfo(full_method=f"/{tool.service_full_name}/{tool.method_name}")

        def inner_handler(request, context):
            return tool.handler(request, context)

        return self._chained_invoke(request, context, info, inner_handler)

    def _chained_invoke(
        self,
        request: Any,
        context: Any,
        info: ServerCallInfo,
        handler: Callable,
    ) -> Any:
        """Run the interceptor chain then call the handler.

        Chain ordering: first registered = outermost (A(B(C(handler)))).
        """
        if not self._interceptors:
            return handler(request, context)

        # Build chain from inside out: wrap handler with interceptors in reverse order.
        def wrap(interceptor, next_handler):
            def wrapped(request, context):
                return interceptor(request, context, info, next_handler)
            return wrapped

        current = handler
        for interceptor in reversed(self._interceptors):
            current = wrap(interceptor, current)

        return current(request, context)

    @classmethod
    def from_descriptor(cls, path: str, *, name: str = "invariant-protocol", version: str = "0.1.0") -> Server:
        """Read a descriptor file and return a configured Server."""
        with open(path, "rb") as f:
            data = f.read()
        return cls.from_bytes(data, name=name, version=version)

    @classmethod
    def from_bytes(cls, data: bytes, *, name: str = "invariant-protocol", version: str = "0.1.0") -> Server:
        """Create a Server from raw FileDescriptorSet bytes."""
        fds = descriptor_pb2.FileDescriptorSet()
        fds.ParseFromString(data)
        parsed = ParsedDescriptor(fds)
        return cls(parsed, name=name, version=version, fds=fds)

    def register(self, servicer: Any, service_name: str | None = None) -> None:
        """Discover methods on servicer that match RPC definitions and register as tools."""
        if service_name is not None:
            svc_info = self.parsed.services.get(service_name)
            if svc_info is None:
                available = list(self.parsed.services.keys())
                raise ValueError(f"Service '{service_name}' not found in descriptor. Available: {available}")
            services = {service_name: svc_info}
        else:
            services = self._match_servicer(servicer)

        for svc_full_name, svc_info in services.items():
            for method_name, method_info in svc_info.methods.items():
                if method_info.client_streaming or method_info.server_streaming:
                    continue

                handler = getattr(servicer, method_name, None)
                if handler is None:
                    continue

                tool_name = f"{svc_info.name}.{method_name}"
                description = method_info.comment or tool_name

                self.tools[tool_name] = Tool(
                    name=tool_name,
                    description=description,
                    input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                    handler=handler,
                    input_type=method_info.input_type,
                    output_type=method_info.output_type,
                    service_full_name=svc_full_name,
                    method_name=method_name,
                )

    def _match_servicer(self, servicer: Any) -> dict:
        """Auto-match a servicer to services by method names."""
        servicer_methods = {m for m in dir(servicer) if not m.startswith("_") and callable(getattr(servicer, m))}
        matched = {}
        for svc_full_name, svc_info in self.parsed.services.items():
            rpc_names = {
                name for name, info in svc_info.methods.items()
                if not info.client_streaming and not info.server_streaming
            }
            if rpc_names and rpc_names & servicer_methods:
                matched[svc_full_name] = svc_info
        if not matched:
            available = list(self.parsed.services.keys())
            raise ValueError(f"No matching service found for servicer. Available: {available}")
        return matched

    def connect(self, target: str, service_name: str | None = None) -> None:
        """Connect to a remote gRPC server and register its methods as tools."""
        channel = grpc.insecure_channel(target)
        self._channels.append(channel)

        pool = descriptor_pool.Default()

        if service_name:
            svc_info = self.parsed.services.get(service_name)
            if svc_info is None:
                available = list(self.parsed.services.keys())
                raise ValueError(f"Service '{service_name}' not found in descriptor. Available: {available}")
            services = {service_name: svc_info}
        else:
            services = self.parsed.services

        for svc_full_name, svc_info in services.items():
            for method_name, method_info in svc_info.methods.items():
                if method_info.client_streaming or method_info.server_streaming:
                    continue

                method_path = f"/{svc_full_name}/{method_name}"
                resp_desc = pool.FindMessageTypeByName(method_info.output_type)
                resp_class = message_factory.GetMessageClass(resp_desc)

                stub = channel.unary_unary(
                    method_path,
                    request_serializer=lambda msg: msg.SerializeToString(),
                    response_deserializer=resp_class.FromString,
                )

                def _make_handler(s):
                    def handler(request, context):
                        return s(request)

                    return handler

                tool_name = f"{svc_info.name}.{method_name}"
                description = method_info.comment or tool_name

                self.tools[tool_name] = Tool(
                    name=tool_name,
                    description=description,
                    input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                    handler=_make_handler(stub),
                    input_type=method_info.input_type,
                    output_type=method_info.output_type,
                    service_full_name=svc_full_name,
                    method_name=method_name,
                )

    def connect_http(self, base_url: str, service_name: str | None = None, *, timeout: float = 10.0) -> None:
        """Connect to a remote HTTP service and register its methods as tools.

        Routes are derived from google.api.http annotations when present, otherwise
        fallback to canonical RPC route: POST /{serviceFullName}/{method}.
        """
        if self._fds is None:
            raise ValueError("connect_http requires Server.from_descriptor() or Server.from_bytes().")

        from invariant.http_client import (
            HTTPDynamicHandler,
            client_binding_for_method,
            http_rules_by_method_path,
        )

        if service_name:
            svc_info = self.parsed.services.get(service_name)
            if svc_info is None:
                available = list(self.parsed.services.keys())
                raise ValueError(f"Service '{service_name}' not found in descriptor. Available: {available}")
            services = {service_name: svc_info}
        else:
            services = self.parsed.services
        rules = http_rules_by_method_path(self._fds)

        for svc_full_name, svc_info in services.items():
            for method_name, method_info in svc_info.methods.items():
                if method_info.client_streaming or method_info.server_streaming:
                    continue

                method_path = f"/{svc_full_name}/{method_name}"
                binding = client_binding_for_method(rules.get(method_path), svc_full_name, method_name)
                handler = HTTPDynamicHandler(
                    base_url=base_url,
                    binding=binding,
                    output_type=method_info.output_type,
                    timeout=timeout,
                )

                tool_name = f"{svc_info.name}.{method_name}"
                description = method_info.comment or tool_name

                self.tools[tool_name] = Tool(
                    name=tool_name,
                    description=description,
                    input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                    handler=handler,
                    input_type=method_info.input_type,
                    output_type=method_info.output_type,
                    service_full_name=svc_full_name,
                    method_name=method_name,
                )

    # -- Public API: single serve method --

    def serve(self, *, mcp: bool = False, cli: bool = False,
              http: int | None = None, grpc: int | None = None,
              grpc_options: list | None = None) -> None:
        """Start the specified projections and block.

        Args:
            grpc_options: Optional list of gRPC server options
                (e.g. ``[("grpc.max_receive_message_length", 1024)]``),
                passed to ``grpc.server()``.

        Examples::

            server.serve(mcp=True)
            server.serve(cli=True)
            server.serve(http=8080)
            server.serve(http=8080, grpc=50051)
            server.serve(grpc=50051, grpc_options=[("grpc.max_receive_message_length", 1024)])
        """
        self._grpc_options = grpc_options
        projections: list[tuple[str, int | None]] = []
        if mcp:
            projections.append(("mcp", None))
        if cli:
            projections.append(("cli", None))
        if http is not None:
            projections.append(("http", http))
        if grpc is not None:
            projections.append(("grpc", grpc))

        if not projections:
            raise ValueError("No projections specified. Use serve(mcp=True), serve(http=8080), etc.")

        if len(projections) == 1:
            self._serve_one(*projections[0])
            return

        import threading

        done = threading.Event()

        for kind, port in projections:
            def run(k=kind, p=port):
                self._serve_one(k, p)
                done.set()
            t = threading.Thread(target=run, daemon=True)
            t.start()

        try:
            done.wait()
        except KeyboardInterrupt:
            pass

    def _serve_one(self, kind: str, port: int | None = None) -> None:
        if kind == "mcp":
            from invariant.projections.mcp import serve_mcp
            serve_mcp(self)
        elif kind == "cli":
            from invariant.projections.cli import run_cli
            result = run_cli(self, sys.argv[1:])
            if isinstance(result, str):
                print(result)
            else:
                # Boundary: proto → JSON (terminal output).
                from google.protobuf import json_format as _jf
                print(_jf.MessageToJson(result, preserving_proto_field_name=True, indent=2))
        elif kind == "http":
            from invariant.projections.http import start_http
            httpd, _ = start_http(self, port)
            httpd.serve_forever()
        elif kind == "grpc":
            from invariant.projections.grpc import start_grpc
            grpc_server, _ = start_grpc(self, port, options=getattr(self, "_grpc_options", None))
            grpc_server.wait_for_termination()

    # -- Non-blocking start/stop (internal, used by tests) --

    def _start_http(self, port: int = 8080) -> int:
        from invariant.projections.http import start_http

        self._http_server, actual_port = start_http(self, port)
        return actual_port

    def _stop_http(self) -> None:
        if self._http_server is not None:
            self._http_server.shutdown()
            self._http_server = None

    def _start_grpc(self, port: int = 50051, *, options: list | None = None) -> int:
        from invariant.projections.grpc import start_grpc

        self._grpc_server, actual_port = start_grpc(self, port, options=options)
        return actual_port

    def _stop_grpc(self) -> None:
        if self._grpc_server is not None:
            self._grpc_server.stop(grace=0)
            self._grpc_server = None

    def _cli(self, args: list[str]) -> dict | str:
        """Run CLI and convert proto result to dict at the boundary (for tests)."""
        from invariant.projections.cli import run_cli

        result = run_cli(self, args)
        if isinstance(result, str):
            return result
        # Boundary: proto → dict.
        from google.protobuf import json_format as _jf
        return _jf.MessageToDict(result, preserving_proto_field_name=True)

    def stop(self) -> None:
        """Close all gRPC channels and stop background servers."""
        self._stop_grpc()
        self._stop_http()
        for ch in self._channels:
            ch.close()
        self._channels.clear()
