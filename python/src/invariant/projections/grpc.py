"""gRPC projection — serve registered tools as a gRPC server."""

from __future__ import annotations

from concurrent import futures
from typing import TYPE_CHECKING

import grpc
from google.protobuf import descriptor_pool, message_factory

if TYPE_CHECKING:
    from invariant.server import Server, Tool


def start_grpc(server: Server, port: int) -> tuple[grpc.Server, int]:
    """Start a gRPC server on the given port and return (server, actual_port)."""
    grpc_server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    grpc_server.add_generic_rpc_handlers([_InvariantHandler(server)])
    actual_port = grpc_server.add_insecure_port(f"[::]:{port}")
    grpc_server.start()
    return grpc_server, actual_port


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

        def handler(request, context):
            return server._invoke(tool, request, context)

        return grpc.unary_unary_rpc_method_handler(
            handler,
            request_deserializer=deserialize,
            response_serializer=serialize,
        )
