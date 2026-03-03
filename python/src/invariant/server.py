"""Invariant Protocol server — register gRPC servicers, project into MCP/CLI/HTTP/gRPC."""

from __future__ import annotations

import json
import sys
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

import grpc
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

from invariant.descriptor import ParsedDescriptor
from invariant.schema import SchemaGenerator


@dataclass
class ServerCallInfo:
    """Mirrors grpc.UnaryServerInfo — metadata passed to interceptors."""

    full_method: str


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

    def __init__(self, parsed: ParsedDescriptor, *, name: str = "invariant-protocol", version: str = "0.1.0"):
        self.parsed = parsed
        self.schema_gen = SchemaGenerator(parsed)
        self.tools: dict[str, Tool] = {}
        self.name = name
        self.version = version
        self._grpc_server = None
        self._http_server = None
        self._channels: list[grpc.Channel] = []
        self._interceptors: list[Callable] = []

    def use(self, interceptor: Callable) -> None:
        """Register an interceptor. Signature: (request, context, info, handler) -> response.

        Interceptors run in registration order (first registered = outermost).
        The ``info`` argument is a :class:`ServerCallInfo` with ``full_method``.
        The ``handler`` is a callable ``(request, context) -> response``.
        """
        self._interceptors.append(interceptor)

    def _invoke(self, tool: Tool, request: Any, context: Any) -> Any:
        """Central dispatch — runs interceptor chain then calls tool.handler.

        Unlike the Go implementation which is JSON-in/JSON-out (centralizing
        serialization), this method is proto-in/proto-out. Each projection
        handles its own JSON ↔ proto conversion before/after calling _invoke().
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
        parsed = ParsedDescriptor.from_file(path)
        return cls(parsed, name=name, version=version)

    @classmethod
    def from_bytes(cls, data: bytes, *, name: str = "invariant-protocol", version: str = "0.1.0") -> Server:
        """Create a Server from raw FileDescriptorSet bytes."""
        fds = descriptor_pb2.FileDescriptorSet()
        fds.ParseFromString(data)
        parsed = ParsedDescriptor(fds)
        return cls(parsed, name=name, version=version)

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

        services = (
            {service_name: self.parsed.services[service_name]}
            if service_name
            else self.parsed.services
        )

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

    # -- Public API: single serve method --

    def serve(self, *, mcp: bool = False, cli: bool = False,
              http: int | None = None, grpc: int | None = None) -> None:
        """Start the specified projections and block.

        Examples::

            server.serve(mcp=True)
            server.serve(cli=True)
            server.serve(http=8080)
            server.serve(http=8080, grpc=50051)
        """
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
            print(json.dumps(result, indent=2))
        elif kind == "http":
            from invariant.projections.http import start_http
            httpd, _ = start_http(self, port)
            httpd.serve_forever()
        elif kind == "grpc":
            from invariant.projections.grpc import start_grpc
            grpc_server, _ = start_grpc(self, port)
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

    def _start_grpc(self, port: int = 50051) -> int:
        from invariant.projections.grpc import start_grpc

        self._grpc_server, actual_port = start_grpc(self, port)
        return actual_port

    def _stop_grpc(self) -> None:
        if self._grpc_server is not None:
            self._grpc_server.stop(grace=0)
            self._grpc_server = None

    def _cli(self, args: list[str]) -> dict:
        from invariant.projections.cli import run_cli

        return run_cli(self, args)

    def stop(self) -> None:
        """Close all gRPC channels and stop background servers."""
        self._stop_grpc()
        self._stop_http()
        for ch in self._channels:
            ch.close()
        self._channels.clear()
