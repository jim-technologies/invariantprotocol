"""Invariant Protocol server — register generated gRPC services and project them.

Async-native end-to-end. All handlers and interceptors are async. Sync handlers
are rejected when the generated ``add_*_Servicer_to_server`` helper runs.
"""

from __future__ import annotations

import asyncio
import copy
import fnmatch
import inspect
import os
import sys
from collections.abc import AsyncIterator, Callable, Coroutine, Mapping, Sequence
from concurrent.futures import Executor
from dataclasses import dataclass, replace
from types import MappingProxyType
from typing import Any, Protocol, cast

import grpc
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

from invariant.descriptor import ParsedDescriptor
from invariant.projection_context import ProjectionContext
from invariant.schema import SchemaGenerator
from invariant.version import package_version


class _RpcMethodHandler(Protocol):
    """Typed surface grpcio exposes on concrete RpcMethodHandler values."""

    request_streaming: bool
    response_streaming: bool
    request_deserializer: Callable[[bytes], Any] | None
    response_serializer: Callable[[Any], bytes] | None
    unary_unary: Callable | None
    unary_stream: Callable | None
    stream_unary: Callable | None
    stream_stream: Callable | None


@dataclass
class _HandlerCallDetails:
    """Concrete structural implementation of grpc.HandlerCallDetails."""

    method: str
    invocation_metadata: Any


@dataclass
class OutboundHTTPRequest:
    """Outbound HTTP request metadata for dynamic header providers."""

    method_path: str
    method: str
    url: str
    body: bytes


HTTPHeaderProvider = Callable[[OutboundHTTPRequest], dict[str, str] | None]

# Selects reviewed HTTP request values to expose as incoming gRPC metadata.
# Invariant applies its reserved identity/authentication filter after this
# callback, so even custom mappers cannot make caller-controlled authorization
# headers trusted.
HTTPMetadataMapper = Callable[[Mapping[str, str]], Sequence[tuple[str, str | bytes]]]

# Returns extra query parameters to add to an outbound request — for APIs that
# authenticate via the query string (an API key, or an HMAC signature +
# timestamp computed over the request). Sees the fully-built request so it can
# sign over the existing query/body. Symmetric to HTTPHeaderProvider.
HTTPQueryProvider = Callable[[OutboundHTTPRequest], dict[str, str] | None]


@dataclass
class OutboundHTTPResponse:
    """Outbound HTTP response metadata for response observers.

    `body` is the raw, undecoded response bytes exactly as received — before any
    proto/JSON parsing — so an observer can archive the verbatim payload (e.g.
    a raw response archive) independent of what the typed message models.
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
class MethodConfig:
    """Per-method HTTP wire limits; zero values inherit server defaults."""

    max_unary_request_bytes: int = 0
    max_unary_response_bytes: int = 0
    max_stream_request_bytes: int = 0
    max_stream_response_bytes: int = 0


@dataclass(frozen=True, slots=True)
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
    request_factory: Callable[[], Any] | None = None
    rpc_handler: _RpcMethodHandler | None = None

    def new_request(self) -> Any:
        """Create the generated request type captured during registration."""
        if self.request_factory is None:
            raise RuntimeError(f"No request factory registered for {self.service_full_name}/{self.method_name}")
        return self.request_factory()


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
        self._tools: dict[str, Tool] = {}
        self._native_tools: dict[tuple[str, str], Tool] = {}
        self._fds = fds
        self._descriptor_pool = _build_descriptor_pool(fds) if fds is not None else None
        self._registered_services: dict[str, dict[str, _RpcMethodHandler]] = {}
        self._http_connections: list[Any] = []
        self._shared_interceptors: list[grpc.aio.ServerInterceptor] = []
        # Body-size safety caps. Defaults are tight; raise per-server when the
        # application has a legitimate need (e.g. accepting large uploads).
        # Mirrors Go's `httpMaxUnaryRequest` / `connectStreamMaxRequest` fields.
        from invariant.projections.http import CONNECT_STREAM_MAX_REQUEST as _STREAM_REQUEST_DEFAULT
        from invariant.projections.http import CONNECT_STREAM_MAX_RESPONSE as _STREAM_RESPONSE_DEFAULT
        from invariant.projections.http import HTTP_MAX_UNARY_REQUEST as _UNARY_REQUEST_DEFAULT
        from invariant.projections.http import HTTP_MAX_UNARY_RESPONSE as _UNARY_RESPONSE_DEFAULT

        self._http_max_unary_request: int = _UNARY_REQUEST_DEFAULT
        self._http_max_unary_response: int = _UNARY_RESPONSE_DEFAULT
        self._connect_stream_max_request: int = _STREAM_REQUEST_DEFAULT
        self._connect_stream_max_response: int = _STREAM_RESPONSE_DEFAULT
        self._method_configs: dict[str, MethodConfig] = {}
        from invariant.projections.http import default_http_metadata_mapper

        self._http_metadata_mapper: HTTPMetadataMapper = default_http_metadata_mapper
        self._includes: list[str] = []
        self._excludes: list[str] = []
        self._registration_started = False
        self._frozen = False
        # Background server handles (test helpers / non-blocking serve).
        self._http_uvicorn: Any = None
        self._http_uvicorn_task: asyncio.Task | None = None
        self._grpc_aio_server: grpc.aio.Server | None = None
        self._grpc_server_built = False

    @property
    def tools(self) -> Mapping[str, Tool]:
        """Return an immutable snapshot of projected tool metadata."""
        snapshot = {
            name: replace(tool, input_schema=copy.deepcopy(tool.input_schema)) for name, tool in self._tools.items()
        }
        return MappingProxyType(snapshot)

    def _freeze(self) -> None:
        self._frozen = True

    def _require_configuration_open(self, subject: str) -> None:
        if self._frozen:
            raise RuntimeError(f"invariant: {subject} cannot be changed after serving or invocation begins")

    def _require_registration_open(self, subject: str) -> None:
        if self._frozen:
            raise RuntimeError(f"invariant: service registration is frozen; cannot register {subject}")

    def _require_projection_filters_open(self, subject: str) -> None:
        self._require_configuration_open(subject)
        if self._registration_started:
            raise RuntimeError(f"invariant: {subject} must be configured before service registration")

    # -- Public API: filtering --

    def include(self, *patterns: str) -> None:
        """Add glob patterns for methods to include. Only methods matching at
        least one include pattern are registered. Patterns match the fully
        qualified path: "service.full.Name.MethodName". "*" matches any chars.
        """
        self._require_projection_filters_open("include filters")
        self._includes.extend(patterns)

    def exclude(self, *patterns: str) -> None:
        """Add glob patterns for methods to exclude. Applied after include."""
        self._require_projection_filters_open("exclude filters")
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

    def use(self, interceptor: grpc.aio.ServerInterceptor) -> None:
        """Register a standard grpc.aio interceptor across every projection.

        First registered is outermost. Constructor interceptors passed to
        :meth:`grpc_server` remain native-only; shared interceptors registered
        here execute inside the typed registered handler exactly once.
        """
        self._require_configuration_open("shared interceptors")
        if not isinstance(interceptor, grpc.aio.ServerInterceptor):
            raise ValueError(f"Interceptor must be grpc.aio.ServerInterceptor, found {interceptor!r}")
        self._shared_interceptors.append(interceptor)

    def set_max_unary_request_bytes(self, n: int) -> None:
        """Override the HTTP unary body-size cap. Pass 0 to reset to the
        16 MiB default. Mirrors Go's ``Server.SetMaxUnaryRequestBytes``.
        """
        self._require_configuration_open("HTTP unary request limit")
        if n < 0:
            raise ValueError("HTTP unary request limit must be non-negative")
        if n == 0:
            from invariant.projections.http import HTTP_MAX_UNARY_REQUEST as _UNARY_DEFAULT

            n = _UNARY_DEFAULT
        self._http_max_unary_request = n

    def set_max_unary_response_bytes(self, n: int) -> None:
        """Override the encoded HTTP unary response cap."""
        self._require_configuration_open("HTTP unary response limit")
        if n < 0:
            raise ValueError("HTTP unary response limit must be non-negative")
        if n == 0:
            from invariant.projections.http import HTTP_MAX_UNARY_RESPONSE

            n = HTTP_MAX_UNARY_RESPONSE
        self._http_max_unary_response = n

    def set_max_stream_request_bytes(self, n: int) -> None:
        """Override the Connect streaming request envelope cap. Pass 0 to
        reset to the 16 MiB default. Mirrors Go's
        ``Server.SetMaxStreamRequestBytes``.
        """
        self._require_configuration_open("HTTP stream request limit")
        if n < 0:
            raise ValueError("HTTP stream request limit must be non-negative")
        if n == 0:
            from invariant.projections.http import CONNECT_STREAM_MAX_REQUEST as _STREAM_DEFAULT

            n = _STREAM_DEFAULT
        self._connect_stream_max_request = n

    def set_max_stream_response_bytes(self, n: int) -> None:
        """Override the per-message encoded Connect response cap."""
        self._require_configuration_open("HTTP stream response limit")
        if n < 0:
            raise ValueError("HTTP stream response limit must be non-negative")
        if n == 0:
            from invariant.projections.http import CONNECT_STREAM_MAX_RESPONSE

            n = CONNECT_STREAM_MAX_RESPONSE
        self._connect_stream_max_response = n

    def configure_method(self, method_path: str, config: MethodConfig) -> None:
        """Override HTTP wire limits for one canonical full gRPC method."""
        self._require_configuration_open("method configuration")
        if any(
            value < 0
            for value in (
                config.max_unary_request_bytes,
                config.max_unary_response_bytes,
                config.max_stream_request_bytes,
                config.max_stream_response_bytes,
            )
        ):
            raise ValueError("Method byte limits must be non-negative")
        self._method_configs[method_path] = config

    def use_http_metadata_mapper(self, mapper: HTTPMetadataMapper | None) -> None:
        """Replace the reviewed inbound HTTP-to-gRPC metadata mapper."""
        self._require_configuration_open("HTTP metadata mapper")
        if mapper is None:
            from invariant.projections.http import default_http_metadata_mapper

            mapper = default_http_metadata_mapper
        self._http_metadata_mapper = mapper

    def _method_limit(self, tool: Tool, field: str, default: int) -> int:
        config = self._method_configs.get(f"/{tool.service_full_name}/{tool.method_name}")
        if config is not None:
            value = getattr(config, field)
            if value > 0:
                return value
        return default

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

        self._freeze()
        tool = self._tools.get(tool_name)
        if tool is None:
            available = sorted(self._tools)
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

        self._freeze()
        tool = self._tools.get(tool_name)
        if tool is None:
            available = sorted(self._tools)
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
        owns_context = context is None
        if owns_context:
            context = ProjectionContext(peer="invariant:invoke")
        full_method = f"/{tool.service_full_name}/{tool.method_name}"
        try:
            try:
                terminal = tool.handler
                if self._shared_interceptors:
                    rpc_handler = await self._intercepted_rpc_handler(full_method, context)
                    if rpc_handler is None:
                        from invariant.errors import InvariantError

                        raise InvariantError(grpc.StatusCode.UNIMPLEMENTED, f"no handler for {full_method}")
                    if (
                        rpc_handler.request_streaming
                        or rpc_handler.response_streaming
                        or rpc_handler.unary_unary is None
                    ):
                        raise TypeError(f"shared interceptor returned no compatible unary handler for {full_method}")
                    terminal = rpc_handler.unary_unary
                response = terminal(request, context)
                if not inspect.isawaitable(response):
                    raise TypeError(f"shared unary handler for {full_method} must be async")
                response = await response
            except Exception as error:
                raise _normalize_handler_error(error, full_method) from error
            if isinstance(context, ProjectionContext):
                context.raise_for_status()
            return response
        finally:
            if owns_context:
                context.finish(cancelled=isinstance(context, ProjectionContext) and context.cancelled())

    async def _invoke_stream(
        self,
        tool: Tool,
        request: Any,
        context: Any,
    ) -> AsyncIterator[Any]:
        """Server-streaming dispatch — yields each response message in order.

        Resolves the registered handler through the shared standard gRPC
        interceptor chain and yields each emitted proto message. Errors raised
        inside the handler or any interceptor propagate to the caller; it is
        the caller's job to translate them to its wire format.
        """
        owns_context = context is None
        if owns_context:
            context = ProjectionContext(peer="invariant:invoke")
        full_method = f"/{tool.service_full_name}/{tool.method_name}"
        try:
            try:
                terminal = tool.handler
                if self._shared_interceptors:
                    rpc_handler = await self._intercepted_rpc_handler(full_method, context)
                    if rpc_handler is None:
                        from invariant.errors import InvariantError

                        raise InvariantError(grpc.StatusCode.UNIMPLEMENTED, f"no handler for {full_method}")
                    if (
                        rpc_handler.request_streaming
                        or not rpc_handler.response_streaming
                        or rpc_handler.unary_stream is None
                    ):
                        raise TypeError(f"shared interceptor returned no compatible stream handler for {full_method}")
                    terminal = rpc_handler.unary_stream
                responses = terminal(request, context)
                if inspect.isawaitable(responses):
                    responses = await responses
                if not hasattr(responses, "__aiter__"):
                    raise TypeError(f"shared stream handler for {full_method} must return an async iterator")
                async for msg in responses:
                    yield msg
            except Exception as error:
                raise _normalize_handler_error(error, full_method) from error
            if isinstance(context, ProjectionContext):
                context.raise_for_status()
        finally:
            if owns_context:
                context.finish(cancelled=isinstance(context, ProjectionContext) and context.cancelled())

    async def _intercepted_rpc_handler(self, full_method: str, context: Any) -> _RpcMethodHandler | None:
        invocation_metadata = ()
        if context is not None:
            metadata_fn = getattr(context, "invocation_metadata", None)
            if callable(metadata_fn):
                metadata = metadata_fn()
                if metadata:
                    invocation_metadata = metadata
        details = _HandlerCallDetails(full_method, invocation_metadata)

        async def dispatch(index: int, call_details: grpc.HandlerCallDetails) -> grpc.RpcMethodHandler:
            if index == len(self._shared_interceptors):
                # grpcio's annotation omits None even though its documented
                # interceptor contract permits a failed handler lookup.
                return cast(grpc.RpcMethodHandler, self._registered_rpc_handler(call_details.method))
            interceptor = self._shared_interceptors[index]

            async def continuation(next_details: grpc.HandlerCallDetails) -> grpc.RpcMethodHandler:
                return await dispatch(index + 1, next_details)

            return await interceptor.intercept_service(continuation, call_details)

        return cast(_RpcMethodHandler | None, await dispatch(0, details))

    def _registered_rpc_handler(self, full_method: str) -> _RpcMethodHandler | None:
        if not isinstance(full_method, str) or not full_method.startswith("/"):
            return None
        service_name, separator, method_name = full_method[1:].partition("/")
        if not separator or not service_name or not method_name:
            return None
        registered = self._registered_services.get(service_name, {}).get(method_name)
        if registered is not None:
            return registered
        tool = self._native_tools.get((service_name, method_name))
        return tool.rpc_handler if tool is not None else None

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

    # -- Public API: generated gRPC registration --

    def add_generic_rpc_handlers(self, _generic_rpc_handlers: Any) -> None:
        """Accept the first half of grpcio's generated registration helper.

        Current generated helpers immediately follow this call with
        :meth:`add_registered_method_handlers`, which exposes the typed method
        map without relying on grpcio's private generic-handler attributes.
        Registration happens only in that second callback, exactly once.
        """
        self._require_registration_open("generated service")

    def add_registered_method_handlers(
        self,
        service_name: str,
        method_handlers: dict[str, _RpcMethodHandler],
    ) -> None:
        """Capture a service from ``add_<Service>Servicer_to_server``.

        The descriptor set remains authoritative for identity and cardinality;
        generated handlers provide the concrete request classes and terminal
        callables used by every projection.
        """
        self._require_registration_open(f"service {service_name!r}")
        svc_info = self.parsed.services.get(service_name)
        if svc_info is None:
            available = sorted(self.parsed.services)
            raise ValueError(f"Service '{service_name}' not found in descriptor. Available: {available}")
        if service_name in self._registered_services:
            raise ValueError(f"Service '{service_name}' is already registered")

        if self._descriptor_pool is None:
            raise ValueError("Generated registration requires Server.from_descriptor() or Server.from_bytes().")
        _validate_generated_service_descriptor(self._descriptor_pool, service_name)

        expected_methods = set(svc_info.methods)
        actual_methods = set(method_handlers)
        if actual_methods != expected_methods:
            missing = sorted(expected_methods - actual_methods)
            extra = sorted(actual_methods - expected_methods)
            raise ValueError(
                f"Generated handlers for '{service_name}' do not match descriptor: missing={missing}, extra={extra}"
            )

        validated: dict[str, tuple[_RpcMethodHandler, Callable[[], Any]]] = {}
        for method_name, method_info in svc_info.methods.items():
            rpc_handler = method_handlers[method_name]
            expected_cardinality = (method_info.client_streaming, method_info.server_streaming)
            actual_cardinality = (rpc_handler.request_streaming, rpc_handler.response_streaming)
            if actual_cardinality != expected_cardinality:
                raise ValueError(
                    f"/{service_name}/{method_name} cardinality does not match descriptor: "
                    f"expected={expected_cardinality}, actual={actual_cardinality}"
                )

            terminal = _rpc_terminal(rpc_handler)
            if method_info.server_streaming:
                if not inspect.isasyncgenfunction(terminal):
                    raise TypeError(
                        f"{service_name}.{method_name} is server-streaming and must be an "
                        "async generator (`async def` with `yield`)."
                    )
            elif not _is_async_callable(terminal):
                raise TypeError(
                    f"{service_name}.{method_name} must be `async def`; invariant-protocol is async-native."
                )

            deserializer = rpc_handler.request_deserializer
            if deserializer is None:
                raise TypeError(f"/{service_name}/{method_name} has no generated request deserializer")
            if rpc_handler.response_serializer is None:
                raise TypeError(f"/{service_name}/{method_name} has no generated response serializer")
            try:
                request = deserializer(b"")
            except Exception as exc:
                raise TypeError(
                    f"/{service_name}/{method_name} request deserializer rejected an empty protobuf message"
                ) from exc
            request_descriptor = getattr(request, "DESCRIPTOR", None)
            actual_input = getattr(request_descriptor, "full_name", None)
            if actual_input != method_info.input_type:
                raise ValueError(
                    f"/{service_name}/{method_name} input type does not match descriptor: "
                    f"expected={method_info.input_type!r}, actual={actual_input!r}"
                )
            request_class = type(request)
            validated[method_name] = (rpc_handler, request_class)

        # Keep the complete generated service for native gRPC, including
        # client-streaming and bidi methods. Only unary and server-streaming
        # methods enter the optional projection catalog below.
        native_tools: list[Tool] = []
        new_tools: list[Tool] = []
        for method_name, method_info in svc_info.methods.items():
            if method_info.client_streaming:
                continue
            rpc_handler, request_factory = validated[method_name]
            terminal = _rpc_terminal(rpc_handler)
            tool_name = f"{svc_info.name}.{method_name}"
            tool = Tool(
                name=tool_name,
                description=method_info.comment or tool_name,
                input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                handler=terminal,
                input_type=method_info.input_type,
                output_type=method_info.output_type,
                service_full_name=service_name,
                method_name=method_name,
                server_streaming=method_info.server_streaming,
                request_factory=request_factory,
                rpc_handler=rpc_handler,
            )
            native_tools.append(tool)
            if self._should_include(service_name, method_name):
                new_tools.append(tool)
        for tool in new_tools:
            existing = self._tools.get(tool.name)
            if existing is not None:
                raise ValueError(
                    f"Tool {tool.name!r} is already registered by {existing.service_full_name!r}; "
                    f"cannot register {tool.service_full_name!r}."
                )
        self._registered_services[service_name] = dict(method_handlers)
        for tool in native_tools:
            self._native_tools[(tool.service_full_name, tool.method_name)] = tool
        for tool in new_tools:
            self._add_tool(tool)
        self._registration_started = True

    def _add_tool(self, tool: Tool) -> None:
        """Register a Tool, rejecting duplicate tool names."""
        existing = self._tools.get(tool.name)
        if existing is not None:
            raise ValueError(
                f"Tool {tool.name!r} is already registered by {existing.service_full_name!r}; "
                f"cannot register {tool.service_full_name!r}."
            )
        self._tools[tool.name] = tool

    def connect_grpc(self, channel: grpc.aio.Channel, service_name: str | None = None) -> None:
        """Register methods on a remote gRPC server as tools.

        Caller builds the channel — use ``grpc.aio.secure_channel`` for production
        with TLS/auth, or ``grpc.aio.insecure_channel`` for local testing. The
        caller owns the channel and closes it after this server has stopped.
        """
        self._require_registration_open("gRPC connection")

        pool = self._require_descriptor_pool("connect_grpc")

        if service_name:
            svc_info = self.parsed.services.get(service_name)
            if svc_info is None:
                available = list(self.parsed.services.keys())
                raise ValueError(f"Service '{service_name}' not found in descriptor. Available: {available}")
            services = {service_name: svc_info}
        else:
            services = self.parsed.services

        duplicate_services = sorted(set(services) & self._registered_services.keys())
        if duplicate_services:
            raise ValueError(f"Services are already registered: {duplicate_services}")

        staged_services: dict[str, dict[str, _RpcMethodHandler]] = {}
        staged_native_tools: list[Tool] = []
        staged_tools: list[Tool] = []
        for svc_full_name, svc_info in services.items():
            service_handlers: dict[str, _RpcMethodHandler] = {}
            for method_name, method_info in svc_info.methods.items():
                if method_info.client_streaming or method_info.server_streaming:
                    continue

                method_path = f"/{svc_full_name}/{method_name}"
                req_desc = pool.FindMessageTypeByName(method_info.input_type)
                req_class = message_factory.GetMessageClass(req_desc)
                resp_desc = pool.FindMessageTypeByName(method_info.output_type)
                resp_class = message_factory.GetMessageClass(resp_desc)

                stub = channel.unary_unary(
                    method_path,
                    request_serializer=lambda msg: msg.SerializeToString(),
                    response_deserializer=resp_class.FromString,
                )

                def _make_handler(s, path):
                    async def remote_handler(request, context):
                        return await _call_remote_unary(s, request, context, path)

                    return remote_handler

                handler = _make_handler(stub, method_path)
                rpc_handler = grpc.unary_unary_rpc_method_handler(
                    handler,
                    request_deserializer=req_class.FromString,
                    response_serializer=lambda msg: msg.SerializeToString(),
                )
                service_handlers[method_name] = rpc_handler
                tool_name = f"{svc_info.name}.{method_name}"
                tool = Tool(
                    name=tool_name,
                    description=method_info.comment or tool_name,
                    input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                    handler=handler,
                    input_type=method_info.input_type,
                    output_type=method_info.output_type,
                    service_full_name=svc_full_name,
                    method_name=method_name,
                    request_factory=req_class,
                    rpc_handler=rpc_handler,
                )
                staged_native_tools.append(tool)
                if self._should_include(svc_full_name, method_name):
                    staged_tools.append(tool)
            if service_handlers:
                staged_services[svc_full_name] = service_handlers

        staged_names: set[str] = set()
        for tool in staged_tools:
            if tool.name in staged_names:
                raise ValueError(f"Tool {tool.name!r} would be registered more than once")
            staged_names.add(tool.name)
            existing = self._tools.get(tool.name)
            if existing is not None:
                raise ValueError(
                    f"Tool {tool.name!r} is already registered by {existing.service_full_name!r}; "
                    f"cannot register {tool.service_full_name!r}."
                )

        self._registered_services.update(staged_services)
        for tool in staged_native_tools:
            self._native_tools[(tool.service_full_name, tool.method_name)] = tool
        for tool in staged_tools:
            self._tools[tool.name] = tool
        self._registration_started = True

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
        self._require_registration_open("HTTP connection")
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
        duplicate_services = sorted(set(services) & self._registered_services.keys())
        if duplicate_services:
            raise ValueError(f"Services are already registered: {duplicate_services}")

        rules = http_rules_by_method_path(self._fds)
        pool = self._require_descriptor_pool("connect_http")
        staged_specs: list[tuple[Any, Any, str, str, str, Any, Any, bool]] = []
        staged_names: set[str] = set()
        for svc_full_name, svc_info in services.items():
            for method_name, method_info in svc_info.methods.items():
                if method_info.client_streaming or method_info.server_streaming:
                    continue

                method_path = f"/{svc_full_name}/{method_name}"
                req_desc = pool.FindMessageTypeByName(method_info.input_type)
                req_class = message_factory.GetMessageClass(req_desc)
                pool.FindMessageTypeByName(method_info.output_type)
                binding = client_binding_for_method(rules.get(method_path), svc_full_name, method_name)
                tool_name = f"{svc_info.name}.{method_name}"
                projected = self._should_include(svc_full_name, method_name)
                if projected:
                    if tool_name in staged_names:
                        raise ValueError(f"Tool {tool_name!r} would be registered more than once")
                    staged_names.add(tool_name)
                    existing = self._tools.get(tool_name)
                    if existing is not None:
                        raise ValueError(
                            f"Tool {tool_name!r} is already registered by {existing.service_full_name!r}; "
                            f"cannot register {svc_full_name!r}."
                        )
                staged_specs.append(
                    (svc_info, method_info, svc_full_name, method_name, method_path, req_class, binding, projected)
                )

        connection = HTTPConnection(
            base_url=base_url,
            auth=auth,
            service_config=service_config,
            options=options,
            observer=observer,
        )
        staged_services: dict[str, dict[str, _RpcMethodHandler]] = {}
        staged_native_tools: list[Tool] = []
        staged_tools: list[Tool] = []
        for (
            svc_info,
            method_info,
            svc_full_name,
            method_name,
            method_path,
            req_class,
            binding,
            projected,
        ) in staged_specs:
            handler = HTTPDynamicHandler(
                connection=connection,
                binding=binding,
                output_type=method_info.output_type,
                method_path=method_path,
                input_type=method_info.input_type,
                pool=pool,
            )
            rpc_handler = grpc.unary_unary_rpc_method_handler(
                handler,
                request_deserializer=req_class.FromString,
                response_serializer=lambda message: message.SerializeToString(),
            )
            staged_services.setdefault(svc_full_name, {})[method_name] = rpc_handler
            tool_name = f"{svc_info.name}.{method_name}"
            tool = Tool(
                name=tool_name,
                description=method_info.comment or tool_name,
                input_schema=self.schema_gen.message_to_schema(method_info.input_type),
                handler=handler,
                input_type=method_info.input_type,
                output_type=method_info.output_type,
                service_full_name=svc_full_name,
                method_name=method_name,
                request_factory=req_class,
                rpc_handler=rpc_handler,
            )
            staged_native_tools.append(tool)
            if projected:
                staged_tools.append(tool)

        self._http_connections.append(connection)
        self._registered_services.update(staged_services)
        for tool in staged_native_tools:
            self._native_tools[(tool.service_full_name, tool.method_name)] = tool
        for tool in staged_tools:
            self._tools[tool.name] = tool
        self._registration_started = True

    # -- Public API: tool catalog --

    def tool_catalog(self) -> list[dict]:
        """Return the canonical tool catalog (same shape as MCP `tools/list`).

        Used by both the HTTP `GET /` endpoint and MCP's `tools/list`.

        Streaming tools carry ``_meta.streaming: True`` so clients can render
        and consume them differently from unary tools. The MCP spec reserves
        ``_meta`` for exactly this kind of server-specific annotation.
        """
        out: list[dict] = []
        for t in sorted(self._tools.values(), key=lambda t: t.name):
            entry: dict = {
                "name": t.name,
                "description": t.description,
                "inputSchema": copy.deepcopy(t.input_schema),
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

        self._freeze()
        return build_asgi_app(self)

    def grpc_server(
        self,
        migration_thread_pool: Executor | None = None,
        *,
        interceptors: Sequence[grpc.aio.ServerInterceptor] | None = None,
        options: Sequence[tuple[str, Any]] | None = None,
        maximum_concurrent_rpcs: int | None = None,
        compression: grpc.Compression | None = None,
    ) -> grpc.aio.Server:
        """Build the one canonical native gRPC server.

        Constructor controls are passed directly to :func:`grpc.aio.server`.
        The caller chooses ports and credentials and starts/waits on the
        returned server. Invariant owns its shutdown through :meth:`stop`.
        """
        if self._grpc_server_built:
            raise RuntimeError("invariant: native gRPC server has already been built")

        from invariant.projections.grpc import _build_grpc_server

        grpc_server = _build_grpc_server(
            self,
            migration_thread_pool=migration_thread_pool,
            interceptors=interceptors,
            options=options,
            maximum_concurrent_rpcs=maximum_concurrent_rpcs,
            compression=compression,
        )
        self._grpc_aio_server = grpc_server
        self._grpc_server_built = True
        return grpc_server

    # -- Public API: optional projections --

    async def serve_projections(
        self,
        *,
        mcp: bool = False,
        cli: bool = False,
        http: int | None = None,
    ) -> None:
        """Run optional HTTP, MCP, and CLI projections until one completes.

        The first projection to finish cancels the others. Native gRPC has an
        independent caller-controlled lifecycle through :meth:`grpc_server`.

        Examples::

            asyncio.run(server.serve_projections(mcp=True))
            asyncio.run(server.serve_projections(http=8080, mcp=True))
        """
        coros: list[Coroutine[Any, Any, None]] = []
        if mcp:
            coros.append(self._serve_mcp())
        if cli:
            coros.append(self._serve_cli())
        if http is not None:
            coros.append(self._serve_http(http))

        if not coros:
            raise ValueError(
                "No projections specified. Use serve_projections(mcp=True) or serve_projections(http=8080)."
            )

        self._freeze()

        if len(coros) == 1:
            await coros[0]
            return

        tasks = [asyncio.create_task(c) for c in coros]
        try:
            done, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
            for task in pending:
                task.cancel()
            await asyncio.gather(*pending, return_exceptions=True)
            for task in tasks:
                if task in done:
                    await task
        finally:
            for task in tasks:
                if not task.done():
                    task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)

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

    async def _start_grpc(
        self,
        port: int = 0,
        *,
        options: Sequence[tuple[str, Any]] | None = None,
    ) -> int:
        """Start a gRPC server in the background and return the bound port."""
        grpc_server = self.grpc_server(options=options)
        actual_port = grpc_server.add_insecure_port(f"[::]:{port}")
        await grpc_server.start()
        return actual_port

    async def _stop_grpc(self, grace: float | None = 0) -> None:
        grpc_server = self._grpc_aio_server
        if grpc_server is None:
            return
        await grpc_server.stop(grace=grace)
        self._grpc_aio_server = None

    async def _cli(self, args: list[str]) -> dict | str:
        """Run CLI and convert proto result to dict at the boundary (for tests)."""
        from invariant.projections.cli import run_cli

        result = await run_cli(self, args)
        if isinstance(result, str):
            return result
        from google.protobuf import json_format as _jf

        return _jf.MessageToDict(result, preserving_proto_field_name=True)

    async def stop(self, grace: float | None = 5.0) -> None:
        """Gracefully stop owned servers and close owned HTTP clients.

        Remote gRPC channels passed to :meth:`connect_grpc` remain caller-owned.
        """
        await self._stop_grpc(grace=grace)
        await self._stop_http()
        for conn in self._http_connections:
            await conn.aclose()
        self._http_connections.clear()

    def _require_descriptor_pool(self, operation: str) -> descriptor_pool.DescriptorPool:
        if self._descriptor_pool is None:
            raise ValueError(f"{operation} requires Server.from_descriptor() or Server.from_bytes().")
        return self._descriptor_pool


def _normalize_handler_error(error: Exception, full_method: str) -> Exception:
    """Preserve declared RPC errors and classify unexpected app failures."""
    from invariant.errors import InvariantError, as_invariant_error

    if isinstance(error, InvariantError | grpc.RpcError):
        return as_invariant_error(error)
    return InvariantError(
        grpc.StatusCode.INTERNAL,
        f"handler failed for {full_method}: {error}",
    )


async def _call_remote_unary(stub: Any, request: Any, context: Any, full_method: str) -> Any:
    """Forward one unary call without losing standard gRPC call semantics."""
    kwargs: dict[str, Any] = {}
    if context is not None:
        invocation_metadata = context.invocation_metadata()
        if invocation_metadata:
            kwargs["metadata"] = tuple(invocation_metadata)
        remaining = context.time_remaining()
        if remaining is not None:
            kwargs["timeout"] = max(0.0, remaining)

    call = stub(request, **kwargs)
    try:
        initial_metadata = await call.initial_metadata()
        await _send_initial_metadata(context, initial_metadata)
        response = await call
        trailing_metadata = await call.trailing_metadata()
        _set_trailing_metadata(context, trailing_metadata)
        return response
    except asyncio.CancelledError:
        call.cancel()
        raise
    except grpc.aio.AioRpcError as error:
        await _send_initial_metadata(context, error.initial_metadata())
        _set_trailing_metadata(context, error.trailing_metadata())
        from invariant.errors import as_invariant_error

        raise as_invariant_error(error) from None
    except Exception as error:
        from invariant.errors import InvariantError

        raise InvariantError(
            grpc.StatusCode.UNAVAILABLE,
            f"gRPC proxy call {full_method} failed: {error}",
        ) from error


async def _send_initial_metadata(context: Any, metadata: Any) -> None:
    if context is None or not metadata:
        return
    try:
        result = context.send_initial_metadata(tuple(metadata))
        if inspect.isawaitable(result):
            await result
    except RuntimeError, grpc.RpcError:
        # An outer interceptor may already have committed headers. gRPC cannot
        # append upstream headers after that point, but the RPC itself remains
        # valid and trailers/status must still propagate.
        return


def _set_trailing_metadata(context: Any, metadata: Any) -> None:
    if context is None or not metadata:
        return
    context.set_trailing_metadata(tuple(metadata))


def _glob_match(pattern: str, s: str) -> bool:
    """Match pattern against string where '*' matches any chars including dots."""
    return fnmatch.fnmatch(s, pattern)


def _rpc_terminal(handler: _RpcMethodHandler) -> Callable:
    """Return the one callable selected by an RPC handler's cardinality."""
    if handler.request_streaming:
        terminal = handler.stream_stream if handler.response_streaming else handler.stream_unary
    else:
        terminal = handler.unary_stream if handler.response_streaming else handler.unary_unary
    if terminal is None:
        raise TypeError(
            "Generated RpcMethodHandler has no terminal for cardinality "
            f"request_streaming={handler.request_streaming}, response_streaming={handler.response_streaming}"
        )
    return terminal


def _build_descriptor_pool(fds: descriptor_pb2.FileDescriptorSet) -> descriptor_pool.DescriptorPool:
    """Build an isolated descriptor registry without relying on pb2 import order."""
    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    pending: dict[str, descriptor_pb2.FileDescriptorProto] = {}
    for file_proto in fds.file:
        if not file_proto.name:
            raise ValueError("FileDescriptorSet contains a file with no name")
        if file_proto.name in pending:
            raise ValueError(f"FileDescriptorSet contains duplicate file {file_proto.name!r}")
        pending[file_proto.name] = file_proto

    errors: dict[str, Exception] = {}
    while pending:
        progressed = False
        for name, file_proto in list(pending.items()):
            try:
                pool.Add(file_proto)
            except (TypeError, ValueError) as error:
                errors[name] = error
                continue
            del pending[name]
            errors.pop(name, None)
            progressed = True
        if not progressed:
            unresolved = ", ".join(f"{name}: {errors[name]}" for name in sorted(pending))
            raise ValueError(f"FileDescriptorSet has unresolved file dependencies: {unresolved}")
    return pool


def _validate_generated_service_descriptor(
    runtime_pool: descriptor_pool.DescriptorPool,
    service_name: str,
) -> None:
    """Reject generated Python bindings compiled from a different graph."""
    generated_pool = descriptor_pool.Default()
    try:
        runtime_service = runtime_pool.FindServiceByName(service_name)
    except KeyError as error:  # pragma: no cover - parsed services already prove this exists
        raise ValueError(f"Service {service_name!r} is absent from descriptor.binpb") from error
    try:
        generated_service = generated_pool.FindServiceByName(service_name)
    except KeyError as error:
        raise ValueError(
            f"Generated protobuf service descriptor {service_name!r} is not linked; "
            "import the generated pb2 module before registration"
        ) from error

    runtime_methods = {method.name: method for method in runtime_service.methods}
    generated_methods = {method.name: method for method in generated_service.methods}
    if runtime_methods.keys() != generated_methods.keys():
        missing = sorted(runtime_methods.keys() - generated_methods.keys())
        extra = sorted(generated_methods.keys() - runtime_methods.keys())
        raise ValueError(
            f"Generated service descriptor for {service_name!r} does not match descriptor.binpb: "
            f"missing={missing}, extra={extra}"
        )
    for method_name, runtime_method in runtime_methods.items():
        generated_method = generated_methods[method_name]
        runtime_cardinality = (runtime_method.client_streaming, runtime_method.server_streaming)
        generated_cardinality = (generated_method.client_streaming, generated_method.server_streaming)
        if generated_cardinality != runtime_cardinality:
            raise ValueError(
                f"/{service_name}/{method_name} generated cardinality does not match descriptor.binpb: "
                f"expected={runtime_cardinality}, actual={generated_cardinality}"
            )
        if generated_method.input_type.full_name != runtime_method.input_type.full_name:
            raise ValueError(
                f"/{service_name}/{method_name} generated input type does not match descriptor.binpb: "
                f"expected={runtime_method.input_type.full_name!r}, actual={generated_method.input_type.full_name!r}"
            )
        if generated_method.output_type.full_name != runtime_method.output_type.full_name:
            raise ValueError(
                f"/{service_name}/{method_name} generated output type does not match descriptor.binpb: "
                f"expected={runtime_method.output_type.full_name!r}, actual={generated_method.output_type.full_name!r}"
            )

    mismatch = _descriptor_graph_mismatch(
        _reachable_service_files(generated_service),
        _reachable_service_files(runtime_service),
    )
    if mismatch:
        raise ValueError(
            f"Generated service descriptor for {service_name!r} protobuf file {mismatch!r} "
            "does not match descriptor.binpb"
        )


def _reachable_service_files(service: Any) -> dict[str, Any]:
    """Return files reached through the service's protobuf value graph.

    Imports used only by compiler options are deliberately excluded. Their
    installed descriptor versions may differ without changing an RPC's values.
    """
    files = {service.file.name: service.file}
    seen_messages: set[str] = set()
    for method in service.methods:
        _add_reachable_message_files(files, seen_messages, method.input_type)
        _add_reachable_message_files(files, seen_messages, method.output_type)
    return files


def _add_reachable_message_files(
    files: dict[str, Any],
    seen_messages: set[str],
    message: Any,
) -> None:
    if message.full_name in seen_messages:
        return
    seen_messages.add(message.full_name)
    files[message.file.name] = message.file
    for field in message.fields:
        if field.message_type is not None:
            _add_reachable_message_files(files, seen_messages, field.message_type)
        elif field.enum_type is not None:
            files[field.enum_type.file.name] = field.enum_type.file


def _descriptor_graph_mismatch(generated: dict[str, Any], runtime: dict[str, Any]) -> str:
    """Return the first differing reachable file, ignoring source comments."""
    if generated.keys() != runtime.keys():
        return "<reachable graph>"
    for path in sorted(generated):
        generated_proto = descriptor_pb2.FileDescriptorProto()
        runtime_proto = descriptor_pb2.FileDescriptorProto()
        generated[path].CopyToProto(generated_proto)
        runtime[path].CopyToProto(runtime_proto)
        generated_proto.ClearField("source_code_info")
        runtime_proto.ClearField("source_code_info")
        if generated_proto != runtime_proto:
            return path
    return ""
