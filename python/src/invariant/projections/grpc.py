"""gRPC projection — async grpc.aio server serving registered tools.

gRPC reflection is enabled by default so grpcurl, Buf Studio, and Connect
debug clients work out of the box.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

import grpc
from google.protobuf import descriptor_pool, message_factory
from grpc_reflection.v1alpha import reflection

from invariant.errors import as_invariant_error

if TYPE_CHECKING:
    from invariant.server import Server, Tool


def build_grpc_server(server: Server, *, options: list | None = None) -> grpc.aio.Server:
    """Build an async gRPC server with all registered tools wired up.

    Caller is responsible for `add_insecure_port`, `start`, `wait_for_termination`.
    """
    grpc_server = grpc.aio.server(options=options)
    grpc_server.add_generic_rpc_handlers([_InvariantHandler(server)])

    # Enable reflection for the registered services.
    service_names = sorted({tool.service_full_name for tool in server.tools.values()})
    if service_names:
        reflection.enable_server_reflection([*service_names, reflection.SERVICE_NAME], grpc_server)

    return grpc_server


class _InvariantHandler(grpc.GenericRpcHandler):
    def __init__(self, server: Server):
        self._server = server
        self._pool = descriptor_pool.Default()
        self._handlers: dict[str, grpc.RpcMethodHandler] = {}
        for tool in server.tools.values():
            key = f"/{tool.service_full_name}/{tool.method_name}"
            self._handlers[key] = self._make_handler(tool)

    def service(self, handler_call_details):
        return self._handlers.get(handler_call_details.method)

    def _make_handler(self, tool: Tool) -> grpc.RpcMethodHandler:
        req_desc = self._pool.FindMessageTypeByName(tool.input_type)
        req_class = message_factory.GetMessageClass(req_desc)
        server = self._server

        def deserialize(data: bytes):
            msg = req_class()
            msg.ParseFromString(data)
            return msg

        def serialize(msg) -> bytes:
            return msg.SerializeToString()

        if tool.server_streaming:

            async def stream_handler(request, context):
                try:
                    async for msg in server._invoke_stream(tool, request, context):
                        yield msg
                except Exception as e:
                    err = as_invariant_error(e)
                    await context.abort(err.code, err.message)

            return grpc.unary_stream_rpc_method_handler(
                stream_handler,
                request_deserializer=deserialize,
                response_serializer=serialize,
            )

        async def handler(request, context):
            try:
                return await server._invoke(tool, request, context)
            except Exception as e:
                err = as_invariant_error(e)
                await context.abort(err.code, err.message)

        return grpc.unary_unary_rpc_method_handler(
            handler,
            request_deserializer=deserialize,
            response_serializer=serialize,
        )
