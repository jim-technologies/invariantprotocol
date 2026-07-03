"""Invariant Protocol server — register gRPC servicers, project into MCP/CLI/HTTP/gRPC.

Async-native end-to-end. All handlers and interceptors are async. Sync handlers
are rejected at register() with a clear error.
"""

from __future__ import annotations

import asyncio
import fnmatch
import inspect
import os
import sys
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass
from typing import Any

import grpc
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

from invariant.descriptor import ParsedDescriptor
from invariant.schema import SchemaGenerator
from invariant.version import package_version

# --- Interceptor types (mirrors gRPC pattern, zero coupling to grpc package) ---

#: Handler is the function called at the end of the interceptor chain.
#: Async-only — must be awaitable.
Handler = Callable[[Any, Any], Awaitable[Any]]

#: Interceptor intercepts unary RPCs across all projections (MCP, HTTP, gRPC, CLI).
#: Async-only — must be awaitable.
Interceptor = Callable[[Any, Any, "ServerCallInfo", Handler], Awaitable[Any]]

#: StreamHandler is the async-generator counterpart to Handler — returns an
#: async iterator of response protos for a server-streaming RPC.
StreamHandler = Callable[[Any, Any], AsyncIterator[Any]]

#: StreamInterceptor wraps server-streaming RPCs. Mirrors Interceptor but
#: returns an async iterator instead of a single value. First registered =
#: outermost. Must be declared as `async def` with `yield` so calling it
#: produces an async iterator.
StreamInterceptor = Callable[[Any, Any, "ServerCallInfo", StreamHandler], AsyncIterator[Any]]


@dataclass
class ServerCallInfo:
    """Metadata about the RPC being invoked, passed to interceptors."""

    full_method: str  # e.g. "/greet.v1.GreetService/Greet"


@dataclass
class OutboundHTTPRequest:
    """Outbound HTTP request metadata for dynamic header providers."""

    method_path: str
    method: str
    url: str
    body: bytes


HTTPHeaderProvider = Callable[[OutboundHTTPRequest], dict[str, str] | None]

# Returns extra query parameters to add to an outbound request — for venues
# that authenticate via the query string (an API key, or an HMAC signature +
# timestamp computed over the request). Sees the fully-built request so it can
# sign over the existing query/body. Symmetric to HTTPHeaderProvider.
HTTPQueryProvider = Callable[[OutboundHTTPRequest], dict[str, str] | None]


@dataclass
class OutboundHTTPResponse:
    """Outbound HTTP response metadata for response observers.

    `body` is the raw, undecoded response bytes exactly as received — before any
    proto/JSON parsing — so an observer can archive the verbatim payload (e.g.
    a raw/bronze data tier) independent of what the typed message models.
    """

    method_path: str
    status_code: int
    headers: dict[str, str]
    body: bytes
    duration_ms: float
    success: bool
    request: OutboundHTTPRequest


# Called once per outbound HTTP response, success or error, after bytes are
# received and before success bodies are parsed into typed messages.
# Side-effecting (archival/metrics); its return value is ignored and exceptions
# are swallowed so an observer can never break the call path.
HTTPResponseObserver = Callable[[OutboundHTTPResponse], None]


@dataclass(slots=True)
class HTTPAuth:
    """Per-connection outbound HTTP credentials.

    Providers are called once per attempt with the fully-built request so
    signatures and timestamps stay fresh across retries.
    """

    header_provider: HTTPHeaderProvider | None = None
    query_provider: HTTPQueryProvider | None = None


@dataclass(slots=True)
class ChannelOptions:
    """Transport options for ``connect_http``.

    Names mirror gRPC channel args where there is a direct HTTP-client analog.
    """

    max_receive_message_size: int = 16 * 1024 * 1024
    connect_timeout: float = 10.0
    read_timeout: float = 10.0
    write_timeout: float = 10.0
    pool_timeout: float = 10.0
    max_connections: int = 100
    max_keepalive_connections: int = 20
    keepalive_expiry: float = 5.0
    proxy: str | None = None
    http2: bool = False


@dataclass(slots=True)
class Tool:
    """A single registered RPC method projected as a tool."""

    name: str
    description: str
    input_schema: dict
    handler: Callable
    input_type: str
    output_type: str
    service_full_name: str
    method_name: str
    server_streaming: bool = False


def _is_async_callable(fn: Any) -> bool:
    """Return True if calling fn(...) returns an awaitable."""
    if inspect.iscoroutinefunction(fn):
        return True
    if not callable(fn):
        return False
    return inspect.iscoroutinefunction(fn.__call__)


_SERVER_NAME = "invariant-protocol"
_SERVER_VERSION = package_version()


class Server:
    """Holds parsed descriptors and registered tools, projects into MCP/CLI/HTTP/gRPC."""

    name = _SERVER_NAME
    version = _SERVER_VERSION

    def __init__(
        self,
        parsed: ParsedDescriptor,
        *,
        fds: descriptor_pb2.FileDescriptorSet | None = None,
    ):
        self.parsed = parsed
        self.schema_gen = SchemaGenerator(parsed)
        self.tools: dict[str, Tool] = {}
        self._fds = fds
        self._channels: list[grpc.aio.Channel] = []
        self._http_connections: list[Any] = []
        self._interceptors: list[Interceptor] = []
        self._stream_interceptors: list[StreamInterceptor] = []
        # Body-size safety caps. Defaults are tight; raise per-server when the
        # application has a legitimate need (e.g. accepting large uploads).
        # Mirrors Go's `httpMaxUnaryRequest` / `connectStreamMaxRequest` fields.
        from invariant.projections.http import CONNECT_STREAM_MAX_REQUEST as _STREAM_DEFAULT
        from invariant.projections.http import HTTP_MAX_UNARY_REQUEST as _UNARY_DEFAULT

        self._http_max_unary_request: int = _UNARY_DEFAULT
        self._connect_stream_max_request: int = _STREAM_DEFAULT
        self._includes: list[str] = []
        self._excludes: list[str] = []
        # Background server handles (test helpers / non-blocking serve).
        self._http_uvicorn: Any = None
        self._http_uvicorn_task: asyncio.Task | None = None
        self._grpc_aio_server: grpc.aio.Server | None = None

    # -- Public API: filtering --

    def include(self, *patterns: str) -> None:
        """Add glob patterns for methods to include. Only methods matching at
        least one include pattern are registered. Patterns match the fully
        qualified path: "service.full.Name.MethodName". "*" matches any chars.
        """
        self._includes.extend(patterns)

    def exclude(self, *patterns: str) -> None:
        """Add glob patterns for methods to exclude. Applied after include."""
        self._excludes.extend(patterns)

    def _should_include(self, service_full_name: str, method_name: str) -> bool:
        full_path = f"{service_full_name}.{method_name}"

        includes = list(self._includes)
        env_include = os.environ.get("INVARIANT_INCLUDE", "")
        if env_include:
            includes.extend(p.strip() for p in env_include.split(",") if p.strip())

        excludes = list(self._excludes)
        env_exclude = os.environ.get("INVARIANT_EXCLUDE", "")
        if env_exclude:
            excludes.extend(p.strip() for p in env_exclude.split(",") if p.strip())

        if includes and not any(_glob_match(p, full_path) for p in includes):
            return False

        return not any(_glob_match(p, full_path) for p in excludes)

    # -- Public API: middleware --

    def use(self, interceptor: Interceptor) -> None:
        """Register an async unary interceptor. First registered = outermost.
        Does not apply to server-streaming RPCs — register a stream interceptor
        separately via ``use_stream``.
        """
        if not _is_async_callable(interceptor):
            raise TypeError(
                f"Interceptor {interceptor!r} must be async (declared with `async def`). "
                "invariant-protocol is async-native."
            )
        self._interceptors.append(interceptor)

    def set_max_unary_request_bytes(self, n: int) -> None:
        """Override the HTTP unary body-size cap. Pass 0 to reset to the
        16 MiB default. Mirrors Go's ``Server.SetMaxUnaryRequestBytes``.
        """
        if n <= 0:
            from invariant.projections.http import HTTP_MAX_UNARY_REQUEST as _UNARY_DEFAULT

            n = _UNARY_DEFAULT
        self._http_max_unary_request = n

    def set_max_stream_request_bytes(self, n: int) -> None:
        """Override the Connect streaming request envelope cap. Pass 0 to
        reset to the 16 MiB default. Mirrors Go's
        ``Server.SetMaxStreamRequestBytes``.
        """
        if n <= 0:
            from invariant.projections.http import CONNECT_STREAM_MAX_REQUEST as _STREAM_DEFAULT

            n = _STREAM_DEFAULT
        self._connect_stream_max_request = n

    def use_stream(self, interceptor: StreamInterceptor) -> None:
        """Register a server-streaming interceptor. Must be an async generator
        function (``async def`` with ``yield``). First registered = outermost.

        Mirrors ``Use`` but for streams — separate because the unary signature
        returns a single value while the stream signature yields multiple.
        """
        if not inspect.isasyncgenfunction(interceptor):
            raise TypeError(
                f"Stream interceptor {interceptor!r} must be an async generator "
                "(declared with `async def` and `yield`)."
            )
        self._stream_interceptors.append(interceptor)

    # -- Public API: invocation core --

    async def invoke(self, tool_name: str, request: Any, context: Any = None) -> Any:
        """Dispatch a unary request to a registered tool by name. Async.

        Returns the proto response. Useful for in-process callers (workflow
        runtimes, tests) that don't need to spin up a projection.

        Raises ``InvariantError(NOT_FOUND)`` if the tool is not registered or
        ``InvariantError(FAILED_PRECONDITION)`` if the tool is server-streaming
        (use ``invoke_stream`` for those). Both errors project to the right
        status code through every projection.
        """
        from invariant.errors import InvariantError

        tool = self.tools.get(tool_name)
        if tool is None:
            available = sorted(self.tools.keys())
            raise InvariantError(grpc.StatusCode.NOT_FOUND, f"Unknown tool '{tool_name}'. Available: {available}")
        if tool.server_streaming:
            raise InvariantError(
                grpc.StatusCode.FAILED_PRECONDITION,
                f"Tool '{tool_name}' is server-streaming — use invoke_stream",
            )
        return await self._invoke(tool, request, context)

    async def invoke_stream(self, tool_name: str, request: Any, context: Any = None) -> AsyncIterator[Any]:
        """Dispatch a server-streaming tool by name. Yields each response message.

        Mirrors ``invoke`` for the streaming case. Same in-process entry point
        used by workflow runtimes and tests — no projection required.
        """
        from invariant.errors import InvariantError

        tool = self.tools.get(tool_name)
        if tool is None:
            available = sorted(self.tools.keys())
            raise InvariantError(grpc.StatusCode.NOT_FOUND, f"Unknown tool '{tool_name}'. Available: {available}")
        if not tool.server_streaming:
            raise InvariantError(
                grpc.StatusCode.FAILED_PRECONDITION,
                f"Tool '{tool_name}' is unary — use invoke",
            )
        async for msg in self._invoke_stream(tool, request, context):
            yield msg

    async def _invoke(self, tool: Tool, request: Any, context: Any) -> Any:
        """Core proto-in/proto-out dispatch. Runs the interceptor chain then
        awaits tool.handler.

        Each projection converts at its boundary:
          - MCP, HTTP: JSON → proto → _invoke → proto → JSON
          - CLI:       input → proto → _invoke → proto (JSON at terminal edge)
          - gRPC:      bytes → proto → _invoke → proto → bytes
        """
        info = ServerCallInfo(full_method=f"/{tool.service_full_name}/{tool.method_name}")
        return await self._chained_invoke(request, context, info, tool.handler)

    async def _chained_invoke(
        self,
        request: Any,
        context: Any,
        info: ServerCallInfo,
        handler: Handler,
    ) -> Any:
        if not self._interceptors:
            return await handler(request, context)

        def wrap(interceptor, next_handler):
            async def wrapped(request, context):
                return await interceptor(request, context, info, next_handler)

            return wrapped

        current = handler
        for interceptor in reversed(self._interceptors):
            current = wrap(interceptor, current)

        return await current(request, context)

    async def _invoke_stream(
        self,
        tool: Tool,
        request: Any,
        context: Any,
    ) -> AsyncIterator[Any]:
        """Server-streaming dispatch — yields each response message in order.

        Wraps the tool's async-generator handler in the registered stream
        interceptors and yields each emitted proto message. Errors raised
        inside the handler or any interceptor propagate to the caller; it
        is the caller's job to translate them to its wire format.
        """
        info = ServerCallInfo(full_method=f"/{tool.service_full_name}/{tool.method_name}")
        handler = tool.handler

        if not self._stream_interceptors:
            async for msg in handler(request, context):
                yield msg
            return

        # Build the chain inside-out: each interceptor wraps the next handler.
        # The interceptor is itself an async-gen function, so calling it returns
        # an async iterator we yield from.
        current = handler
        for interceptor in reversed(self._stream_interceptors):
            current = _wrap_stream_interceptor(interceptor, current, info)

        async for msg in current(request, context):
            yield msg

    # -- Public API: construction --

    @classmethod
    def from_descriptor(cls, path: str) -> Server:
        """Read a descriptor file and return a configured Server."""
        with open(path, "rb") as f:
            data = f.read()
        return cls.from_bytes(data)

    @classmethod
    def from_bytes(cls, data: bytes) -> Server:
        """Create a Server from raw FileDescriptorSet bytes."""
        fds = descriptor_pb2.FileDescriptorSet()
        fds.ParseFromString(data)
        parsed = ParsedDescriptor(fds)
        return cls(parsed, fds=fds)

    # -- Public API: registration --

    def register(self, servicer: Any, service_name: str | None = None) -> None:
        """Discover async methods on servicer that match RPC definitions and register as tools.

        Sync handlers are rejected — declare methods as `async def`.
        """
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
                # Client-streaming and bidi are out of scope — opinionated.
                if method_info.client_streaming:
                    continue

                if not self._should_include(svc_full_name, method_name):
                    continue

                handler = getattr(servicer, method_name, None)
                if handler is None:
                    continue

                if method_info.server_streaming:
                    if not inspect.isasyncgenfunction(handler):
                        raise TypeError(
                            f"{type(servicer).__name__}.{method_name} is a server-streaming RPC "
                            "and must be an async generator (`async def` with `yield`)."
                        )
                elif not _is_async_callable(handler):
                    raise TypeError(
                        f"{type(servicer).__name__}.{method_name} must be `async def`. "
                        "invariant-protocol is async-native."
                    )

                tool_name = f"{svc_info.name}.{method_name}"
                self._add_tool(
                    Tool(
                        name=tool_name,
                        description=method_info.comment or tool_name,
                        input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                        handler=handler,
                        input_type=method_info.input_type,
                        output_type=method_info.output_type,
                        service_full_name=svc_full_name,
                        method_name=method_name,
                        server_streaming=method_info.server_streaming,
                    )
                )

    def _add_tool(self, tool: Tool) -> None:
        """Register a Tool, rejecting collisions with an already-registered tool name."""
        existing = self.tools.get(tool.name)
        if existing is not None and existing.service_full_name != tool.service_full_name:
            raise ValueError(
                f"Tool name collision: {tool.name!r} is registered by both "
                f"{existing.service_full_name!r} and {tool.service_full_name!r}. "
                "Two services in different packages share the same simple name; "
                "use Server.include() to scope to one."
            )
        self.tools[tool.name] = tool

    def _match_servicer(self, servicer: Any) -> dict:
        """Auto-match a servicer to services by method names.

        Considers unary and server-streaming methods — both are supported tool
        shapes. Client-streaming and bidi RPCs are filtered out at registration
        time anyway, so we don't gate matching on them either.
        """
        servicer_methods = {m for m in dir(servicer) if not m.startswith("_") and callable(getattr(servicer, m))}
        matched = {}
        for svc_full_name, svc_info in self.parsed.services.items():
            rpc_names = {name for name, info in svc_info.methods.items() if not info.client_streaming}
            if rpc_names and rpc_names & servicer_methods:
                matched[svc_full_name] = svc_info
        if not matched:
            available = list(self.parsed.services.keys())
            raise ValueError(f"No matching service found for servicer. Available: {available}")
        return matched

    def connect(self, channel: grpc.aio.Channel, service_name: str | None = None) -> None:
        """Register methods on a remote gRPC server as tools.

        Caller builds the channel — use ``grpc.aio.secure_channel`` for production
        with TLS/auth, or ``grpc.aio.insecure_channel`` for local testing. The
        Server takes ownership of closing the channel on ``stop()``.
        """
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

                if not self._should_include(svc_full_name, method_name):
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
                    async def handler(request, context):
                        return await s(request)

                    return handler

                tool_name = f"{svc_info.name}.{method_name}"
                self._add_tool(
                    Tool(
                        name=tool_name,
                        description=method_info.comment or tool_name,
                        input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                        handler=_make_handler(stub),
                        input_type=method_info.input_type,
                        output_type=method_info.output_type,
                        service_full_name=svc_full_name,
                        method_name=method_name,
                    )
                )

    def connect_http(
        self,
        base_url: str,
        service_name: str | None = None,
        *,
        auth: HTTPAuth | HTTPHeaderProvider | None = None,
        service_config: dict[str, Any] | None = None,
        options: ChannelOptions | None = None,
        observer: HTTPResponseObserver | None = None,
    ) -> None:
        """Connect to a remote HTTP service and register its methods as tools.

        Routes are derived from google.api.http annotations when present, otherwise
        fallback to canonical RPC route: POST /{serviceFullName}/{method}.
        """
        if self._fds is None:
            raise ValueError("connect_http requires Server.from_descriptor() or Server.from_bytes().")

        from invariant.http_client import (
            HTTPConnection,
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
        connection = HTTPConnection(
            base_url=base_url,
            auth=auth,
            service_config=service_config,
            options=options,
            observer=observer,
        )
        self._http_connections.append(connection)

        for svc_full_name, svc_info in services.items():
            for method_name, method_info in svc_info.methods.items():
                if method_info.client_streaming or method_info.server_streaming:
                    continue

                if not self._should_include(svc_full_name, method_name):
                    continue

                method_path = f"/{svc_full_name}/{method_name}"
                binding = client_binding_for_method(rules.get(method_path), svc_full_name, method_name)
                handler = HTTPDynamicHandler(
                    connection=connection,
                    binding=binding,
                    output_type=method_info.output_type,
                    method_path=method_path,
                    input_type=method_info.input_type,
                )

                tool_name = f"{svc_info.name}.{method_name}"
                self._add_tool(
                    Tool(
                        name=tool_name,
                        description=method_info.comment or tool_name,
                        input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                        handler=handler,
                        input_type=method_info.input_type,
                        output_type=method_info.output_type,
                        service_full_name=svc_full_name,
                        method_name=method_name,
                    )
                )

    # -- Public API: tool catalog --

    def tool_catalog(self) -> list[dict]:
        """Return the canonical tool catalog (same shape as MCP `tools/list`).

        Used by both the HTTP `GET /` endpoint and MCP's `tools/list`.

        Streaming tools carry ``_meta.streaming: True`` so clients can render
        and consume them differently from unary tools. The MCP spec reserves
        ``_meta`` for exactly this kind of server-specific annotation.
        """
        out: list[dict] = []
        for t in sorted(self.tools.values(), key=lambda t: t.name):
            entry: dict = {
                "name": t.name,
                "description": t.description,
                "inputSchema": t.input_schema,
            }
            if t.server_streaming:
                entry["_meta"] = {"streaming": True}
            out.append(entry)
        return out

    # -- Public API: ASGI mounting --

    def asgi_app(self):
        """Return the ASGI application that serves all registered tools over HTTP.

        Mount on an existing ASGI server (uvicorn, hypercorn, FastAPI, Starlette,
        etc.) instead of binding a separate port::

            app = server.asgi_app()
            # or compose with another framework's router:
            asgi_application.mount("/inv", app)
        """
        from invariant.projections.http import build_asgi_app

        return build_asgi_app(self)

    # -- Public API: serve --

    async def serve(
        self,
        *,
        mcp: bool = False,
        cli: bool = False,
        http: int | None = None,
        grpc: int | None = None,
    ) -> None:
        """Start the specified projections and block until cancelled.

        Cancelling any projection (or the parent task) cancels all of them.
        For custom gRPC ServerOptions, compose `build_grpc_server(self, options=...)`
        and manage lifecycle yourself.

        Examples::

            asyncio.run(server.serve(mcp=True))
            asyncio.run(server.serve(http=8080, grpc=50051))
        """
        coros: list[Awaitable[None]] = []
        if mcp:
            coros.append(self._serve_mcp())
        if cli:
            coros.append(self._serve_cli())
        if http is not None:
            coros.append(self._serve_http(http))
        if grpc is not None:
            coros.append(self._serve_grpc(grpc))

        if not coros:
            raise ValueError("No projections specified. Use serve(mcp=True), serve(http=8080), etc.")

        if len(coros) == 1:
            await coros[0]
            return

        tasks = [asyncio.create_task(c) for c in coros]
        try:
            await asyncio.gather(*tasks)
        except BaseException:
            for t in tasks:
                t.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)
            raise

    # -- Per-projection serve coroutines --

    async def _serve_mcp(self) -> None:
        from invariant.projections.mcp import serve_mcp

        await serve_mcp(self)

    async def _serve_cli(self) -> None:
        from invariant.projections.cli import stream_cli

        def write(piece: str) -> None:
            # flush=True so streamed chunks reach the consumer immediately —
            # without it Python buffers stdout when piped, defeating streaming.
            print(piece, flush=True)

        await stream_cli(self, sys.argv[1:], write)

    async def _serve_http(self, port: int) -> None:
        import uvicorn

        config = uvicorn.Config(
            self.asgi_app(),
            host="0.0.0.0",  # noqa: S104
            port=port,
            log_level="warning",
        )
        server = uvicorn.Server(config)
        self._http_uvicorn = server
        try:
            await server.serve()
        finally:
            self._http_uvicorn = None

    async def _serve_grpc(self, port: int) -> None:
        from invariant.projections.grpc import build_grpc_server

        grpc_server = build_grpc_server(self)
        grpc_server.add_insecure_port(f"[::]:{port}")
        await grpc_server.start()
        self._grpc_aio_server = grpc_server
        try:
            await grpc_server.wait_for_termination()
        except asyncio.CancelledError:
            # Graceful shutdown: let in-flight RPCs finish (5s grace) before
            # closing the listening socket.
            await grpc_server.stop(grace=5)
            raise
        finally:
            self._grpc_aio_server = None

    # -- Test helpers (non-blocking start/stop) --

    async def _start_http(self, port: int = 0) -> int:
        """Start an HTTP server in the background and return the bound port."""
        import uvicorn

        config = uvicorn.Config(
            self.asgi_app(),
            host="127.0.0.1",
            port=port,
            log_level="warning",
        )
        server = uvicorn.Server(config)
        self._http_uvicorn = server
        self._http_uvicorn_task = asyncio.create_task(server.serve())

        # Wait for uvicorn to bind. server.servers is populated after bind.
        for _ in range(200):
            if server.started and server.servers:
                socket = server.servers[0].sockets[0]
                return socket.getsockname()[1]
            await asyncio.sleep(0.01)
        raise RuntimeError("HTTP server failed to bind within 2 seconds")

    async def _stop_http(self) -> None:
        if self._http_uvicorn is None:
            return
        self._http_uvicorn.should_exit = True
        if self._http_uvicorn_task is not None:
            await asyncio.gather(self._http_uvicorn_task, return_exceptions=True)
            self._http_uvicorn_task = None
        self._http_uvicorn = None

    async def _start_grpc(self, port: int = 0, *, options: list | None = None) -> int:
        """Start a gRPC server in the background and return the bound port."""
        from invariant.projections.grpc import build_grpc_server

        grpc_server = build_grpc_server(self, options=options)
        actual_port = grpc_server.add_insecure_port(f"[::]:{port}")
        await grpc_server.start()
        self._grpc_aio_server = grpc_server
        return actual_port

    async def _stop_grpc(self) -> None:
        if self._grpc_aio_server is not None:
            await self._grpc_aio_server.stop(grace=0)
            self._grpc_aio_server = None

    async def _cli(self, args: list[str]) -> dict | str:
        """Run CLI and convert proto result to dict at the boundary (for tests)."""
        from invariant.projections.cli import run_cli

        result = await run_cli(self, args)
        if isinstance(result, str):
            return result
        from google.protobuf import json_format as _jf

        return _jf.MessageToDict(result, preserving_proto_field_name=True)

    async def stop(self) -> None:
        """Close all gRPC channels, HTTP clients, and stop background servers."""
        await self._stop_grpc()
        await self._stop_http()
        for ch in self._channels:
            await ch.close()
        self._channels.clear()
        for conn in self._http_connections:
            await conn.aclose()
        self._http_connections.clear()
        for tool in self.tools.values():
            aclose = getattr(tool.handler, "aclose", None)
            if aclose is not None:
                await aclose()


def _glob_match(pattern: str, s: str) -> bool:
    """Match pattern against string where '*' matches any chars including dots."""
    return fnmatch.fnmatch(s, pattern)


def _wrap_stream_interceptor(
    interceptor: StreamInterceptor,
    next_handler: StreamHandler,
    info: ServerCallInfo,
) -> StreamHandler:
    """Wrap next_handler with interceptor — both are async-gen functions."""

    async def wrapped(request, context):
        async for msg in interceptor(request, context, info, next_handler):
            yield msg

    return wrapped
