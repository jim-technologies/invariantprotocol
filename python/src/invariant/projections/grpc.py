"""gRPC projection — async grpc.aio server serving registered tools.

gRPC reflection is enabled by default so grpcurl, Buf Studio, and Connect
debug clients work out of the box.
"""

from __future__ import annotations

import inspect
from collections.abc import Callable, Sequence
from concurrent.futures import Executor
from typing import TYPE_CHECKING, Any

import grpc
from google.protobuf import descriptor_pb2, descriptor_pool
from grpc_reflection.v1alpha import reflection, reflection_pb2

from invariant.errors import InvariantError, as_invariant_error

if TYPE_CHECKING:
    from invariant.server import Server, _Tool


def _build_grpc_server(
    server: Server,
    migration_thread_pool: Executor | None = None,
    *,
    interceptors: Sequence[grpc.aio.ServerInterceptor] | None = None,
    options: Sequence[tuple[str, Any]] | None = None,
    maximum_concurrent_rpcs: int | None = None,
    compression: grpc.Compression | None = None,
) -> grpc.aio.Server:
    """Build an async gRPC server with all registered tools wired up.

    Caller is responsible for `add_insecure_port`, `start`, `wait_for_termination`.
    """
    server._freeze()
    grpc_server = grpc.aio.server(
        migration_thread_pool=migration_thread_pool,
        interceptors=interceptors,
        options=options,
        maximum_concurrent_rpcs=maximum_concurrent_rpcs,
        compression=compression,
    )
    projected_handlers = _projected_handlers(server)
    for service_name, method_handlers in projected_handlers.items():
        generic_handler = grpc.method_handlers_generic_handler(service_name, method_handlers)
        grpc_server.add_generic_rpc_handlers((generic_handler,))
        # Available since grpcio 1.32; the third-party typing package does not
        # yet expose it on the async Server protocol.
        grpc_server.add_registered_method_handlers(service_name, method_handlers)  # type: ignore[attr-defined]

    # Enable reflection for the registered services.
    reflected_methods = _reflected_methods(server)
    service_names = sorted(reflected_methods)
    reflection.enable_server_reflection(
        [*service_names, reflection.SERVICE_NAME],
        grpc_server,
        pool=_reflection_pool(server, reflected_methods),
    )

    return grpc_server


def _reflected_methods(server: Server) -> dict[str, set[str]]:
    reflected = {service: set(methods) for service, methods in server._registered_services.items()}
    for service, method in server._native_tools:
        reflected.setdefault(service, set()).add(method)
    return reflected


def _reflection_pool(
    server: Server,
    reflected_methods: dict[str, set[str]],
) -> descriptor_pool.DescriptorPool:
    """Build reflection lookup state without the process-global descriptor pool."""
    from invariant.server import _build_descriptor_pool

    files = descriptor_pb2.FileDescriptorSet()
    if server._fds is not None:
        files.CopyFrom(server._fds)
    _filter_reflected_services(files, reflected_methods)
    retained = [file for file in files.file if file.name != reflection_pb2.DESCRIPTOR.name]
    del files.file[:]
    files.file.extend(retained)
    reflection_pb2.DESCRIPTOR.CopyToProto(files.file.add())
    return _build_descriptor_pool(files)


def _filter_reflected_services(
    files: descriptor_pb2.FileDescriptorSet,
    reflected_methods: dict[str, set[str]],
) -> None:
    for file in files.file:
        service_indexes: dict[int, int] = {}
        method_indexes: dict[int, dict[int, int]] = {}
        kept_services: list[descriptor_pb2.ServiceDescriptorProto] = []
        for old_service_index, service in enumerate(file.service):
            full_name = f"{file.package}.{service.name}" if file.package else service.name
            allowed_methods = reflected_methods.get(full_name)
            if not allowed_methods:
                continue

            kept_service = descriptor_pb2.ServiceDescriptorProto()
            kept_service.CopyFrom(service)
            del kept_service.method[:]
            kept_method_indexes: dict[int, int] = {}
            for old_method_index, method in enumerate(service.method):
                if method.name not in allowed_methods:
                    continue
                kept_method_indexes[old_method_index] = len(kept_service.method)
                kept_service.method.add().CopyFrom(method)

            if not kept_service.method:
                continue
            service_indexes[old_service_index] = len(kept_services)
            method_indexes[old_service_index] = kept_method_indexes
            kept_services.append(kept_service)

        del file.service[:]
        file.service.extend(kept_services)

        kept_locations: list[descriptor_pb2.SourceCodeInfo.Location] = []
        for location in file.source_code_info.location:
            kept = descriptor_pb2.SourceCodeInfo.Location()
            kept.CopyFrom(location)
            path = list(kept.path)
            if len(path) >= 2 and path[0] == 6:
                old_service_index = path[1]
                if old_service_index not in service_indexes:
                    continue
                kept.path[1] = service_indexes[old_service_index]
                if len(path) >= 4 and path[2] == 2:
                    old_method_index = path[3]
                    new_method_index = method_indexes[old_service_index].get(old_method_index)
                    if new_method_index is None:
                        continue
                    kept.path[3] = new_method_index
            kept_locations.append(kept)
        del file.source_code_info.location[:]
        file.source_code_info.location.extend(kept_locations)


def _projected_handlers(server: Server) -> dict[str, dict[str, grpc.RpcMethodHandler]]:
    """Wrap generated terminals for shared dispatch without changing codecs."""
    projected: dict[str, dict[str, grpc.RpcMethodHandler]] = {}
    tools = server._native_tools
    for service_name, registered in server._registered_services.items():
        methods: dict[str, Any] = {}
        for method_name, rpc_handler in registered.items():
            tool = tools.get((service_name, method_name))
            if tool is not None:
                methods[method_name] = _wrap_tool(server, tool)
            elif server._shared_interceptors:
                # Client-streaming and bidi methods are native-only, but shared
                # standard interceptors still apply once to the complete
                # generated service just as grpc-go stream interceptors do.
                methods[method_name] = _wrap_native_streaming_handler(
                    server,
                    f"/{service_name}/{method_name}",
                    rpc_handler,
                )
            else:
                methods[method_name] = rpc_handler
        projected[service_name] = methods
    for tool in server._tools.values():
        methods = projected.setdefault(tool.service_full_name, {})
        methods.setdefault(tool.method_name, _wrap_tool(server, tool))
    return projected


def _wrap_tool(server: Server, tool: _Tool) -> grpc.RpcMethodHandler:
    generated = tool.rpc_handler
    request_deserializer: Callable[[bytes], Any] | None
    response_serializer: Callable[[Any], bytes] | None
    if generated is None:

        def deserialize(data: bytes):
            request = tool.new_request()
            request.ParseFromString(data)
            return request

        def serialize(response) -> bytes:
            return response.SerializeToString()

        request_deserializer = deserialize
        response_serializer = serialize
    else:
        request_deserializer = generated.request_deserializer
        response_serializer = generated.response_serializer

    if tool.server_streaming:

        async def stream_handler(request, context):
            try:
                async for msg in server._invoke_stream(tool, request, context):
                    yield msg
            except Exception as e:
                err = as_invariant_error(e)
                await context.abort(err.code, err.message, trailing_metadata=err.grpc_trailing_metadata())

        return grpc.unary_stream_rpc_method_handler(
            stream_handler,
            request_deserializer=request_deserializer,
            response_serializer=response_serializer,
        )

    async def handler(request, context):
        try:
            return await server._invoke(tool, request, context)
        except Exception as e:
            err = as_invariant_error(e)
            await context.abort(err.code, err.message, trailing_metadata=err.grpc_trailing_metadata())

    return grpc.unary_unary_rpc_method_handler(
        handler,
        request_deserializer=request_deserializer,
        response_serializer=response_serializer,
    )


def _wrap_native_streaming_handler(server: Server, full_method: str, generated: Any) -> grpc.RpcMethodHandler:
    """Apply shared aio interceptors to native client-streaming cardinalities."""
    if not generated.request_streaming:
        raise TypeError(f"{full_method} has no projected tool but is not client-streaming")

    if generated.response_streaming:

        async def stream_stream_handler(request_iterator, context):
            try:
                resolved = await server._intercepted_rpc_handler(full_method, context)
                if (
                    resolved is None
                    or not resolved.request_streaming
                    or not resolved.response_streaming
                    or resolved.stream_stream is None
                ):
                    raise TypeError(f"shared interceptor returned no compatible bidi handler for {full_method}")
                responses = resolved.stream_stream(request_iterator, context)
                if inspect.isawaitable(responses):
                    responses = await responses
                if not hasattr(responses, "__aiter__"):
                    raise TypeError(f"shared bidi handler for {full_method} must return an async iterator")
                async for response in responses:
                    yield response
            except Exception as error:
                await _abort_handler_error(context, error, full_method)

        return grpc.stream_stream_rpc_method_handler(
            stream_stream_handler,
            request_deserializer=generated.request_deserializer,
            response_serializer=generated.response_serializer,
        )

    async def stream_unary_handler(request_iterator, context):
        try:
            resolved = await server._intercepted_rpc_handler(full_method, context)
            if (
                resolved is None
                or not resolved.request_streaming
                or resolved.response_streaming
                or resolved.stream_unary is None
            ):
                raise TypeError(f"shared interceptor returned no compatible client-streaming handler for {full_method}")
            response = resolved.stream_unary(request_iterator, context)
            if not inspect.isawaitable(response):
                raise TypeError(f"shared client-streaming handler for {full_method} must be async")
            return await response
        except Exception as error:
            await _abort_handler_error(context, error, full_method)

    return grpc.stream_unary_rpc_method_handler(
        stream_unary_handler,
        request_deserializer=generated.request_deserializer,
        response_serializer=generated.response_serializer,
    )


async def _abort_handler_error(context: Any, error: Exception, full_method: str) -> None:
    if isinstance(error, InvariantError | grpc.RpcError):
        rpc_error = as_invariant_error(error)
    else:
        rpc_error = InvariantError(
            grpc.StatusCode.INTERNAL,
            f"handler failed for {full_method}: {error}",
        )
    await context.abort(
        rpc_error.code,
        rpc_error.message,
        trailing_metadata=rpc_error.grpc_trailing_metadata(),
    )
