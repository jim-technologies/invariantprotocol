"""Test interceptor middleware across all projections."""

import json
import os
import subprocess
import sys

import greet_pb2
import grpc
import httpx
import pytest
from google.protobuf import descriptor_pool, message_factory


def _shared_unary_interceptor(callback):
    class SharedUnaryInterceptor(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            handler = await continuation(handler_call_details)
            assert handler is not None
            terminal = handler.unary_unary
            assert terminal is not None

            async def wrapped(request, context):
                return await callback(request, context, handler_call_details, terminal)

            return grpc.unary_unary_rpc_method_handler(
                wrapped,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

    return SharedUnaryInterceptor()


async def test_interceptor_fires_on_cli(server):
    log = []

    async def interceptor(request, context, _details, handler):
        log.append("A-before")
        resp = await handler(request, context)
        log.append("A-after")
        return resp

    server.use(_shared_unary_interceptor(interceptor))
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "CLI"}'])
    assert result["message"] == "Hi CLI"
    assert log == ["A-before", "A-after"]


async def test_interceptor_fires_on_http(server):
    log = []
    calls = []

    async def interceptor(request, context, details, handler):
        log.append("A-before")
        calls.append((type(request), details.method, tuple(details.invocation_metadata)))
        resp = await handler(request, context)
        log.append("A-after")
        return resp

    server.use(_shared_unary_interceptor(interceptor))
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "HTTP"},
                headers={"x-request-id": "request-123"},
            )
        assert resp.json()["message"] == "Hi HTTP"
        assert log == ["A-before", "A-after"]
        assert calls[0][1:] == (
            "/greet.v1.GreetService/Greet",
            (("x-request-id", "request-123"),),
        )
        assert calls[0][0] is greet_pb2.GreetRequest
    finally:
        await server._stop_http()


async def test_interceptor_fires_on_grpc(server):
    log = []

    async def interceptor(request, context, _details, handler):
        log.append("A-before")
        resp = await handler(request, context)
        log.append("A-after")
        return resp

    server.use(_shared_unary_interceptor(interceptor))
    port = await server._start_grpc(port=0)
    try:
        pool = descriptor_pool.Default()
        req_class = message_factory.GetMessageClass(pool.FindMessageTypeByName("greet.v1.GreetRequest"))
        resp_class = message_factory.GetMessageClass(pool.FindMessageTypeByName("greet.v1.GreetResponse"))

        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = channel.unary_unary(
                "/greet.v1.GreetService/Greet",
                request_serializer=lambda msg: msg.SerializeToString(),
                response_deserializer=resp_class.FromString,
            )
            request = req_class()
            request.name = "gRPC"
            response = await stub(request)
        assert response.message == "Hi gRPC"
        assert log == ["A-before", "A-after"]
    finally:
        await server._stop_grpc()


async def test_interceptor_chain_order(server):
    log = []

    def make_interceptor(label):
        async def interceptor(request, context, _details, handler):
            log.append(f"{label}-before")
            resp = await handler(request, context)
            log.append(f"{label}-after")
            return resp

        return interceptor

    server.use(_shared_unary_interceptor(make_interceptor("A")))
    server.use(_shared_unary_interceptor(make_interceptor("B")))
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "Order"}'])
    assert result["message"] == "Hi Order"
    assert log == ["A-before", "B-before", "B-after", "A-after"]


async def test_interceptor_short_circuit(server):
    from invariant import InvariantError

    async def blocking_interceptor(request, context, _details, handler):
        del request, context, handler
        raise ValueError("blocked by interceptor")

    server.use(_shared_unary_interceptor(blocking_interceptor))
    with pytest.raises(InvariantError, match=r"/greet\.v1\.GreetService/Greet.*blocked by interceptor") as exc:
        await server._cli(["GreetService", "Greet", "-r", '{"name": "Blocked"}'])
    assert exc.value.code == grpc.StatusCode.INTERNAL


async def test_standard_interceptor_can_supply_handler_without_continuation(server):
    class ShortCircuit(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            del continuation, handler_call_details

            async def terminal(request, context):
                del context
                return greet_pb2.GreetResponse(message=f"intercepted {request.name}")

            return grpc.unary_unary_rpc_method_handler(terminal)

    server.use(ShortCircuit())
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "Ada"}'])
    assert result["message"] == "intercepted Ada"


async def test_standard_interceptor_missing_handler_is_unimplemented(server):
    from invariant import InvariantError

    class Missing(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            del continuation, handler_call_details

    server.use(Missing())
    with pytest.raises(InvariantError, match="no handler") as exc:
        await server._cli(["GreetService", "Greet", "-r", '{"name": "Ada"}'])
    assert exc.value.code == grpc.StatusCode.UNIMPLEMENTED


async def test_interceptor_full_method(server):
    captured = {}

    async def interceptor(request, context, details, handler):
        captured["full_method"] = details.method
        return await handler(request, context)

    server.use(_shared_unary_interceptor(interceptor))
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "Method"}'])
    assert result["message"] == "Hi Method"
    assert captured["full_method"] == "/greet.v1.GreetService/Greet"


async def test_no_interceptors(server):
    assert len(server._shared_interceptors) == 0
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "Ada"}'])
    assert result["message"] == "Hi Ada"


def test_interceptor_rejects_non_grpc_interceptor():
    from invariant import Server

    srv = Server.from_descriptor(os.path.join(os.path.dirname(os.path.abspath(__file__)), "proto", "descriptor.binpb"))

    def sync_interceptor(request, context, info, handler):
        return handler(request, context)

    with pytest.raises(ValueError, match=r"grpc\.aio\.ServerInterceptor"):
        srv.use(sync_interceptor)


def test_interceptor_fires_on_mcp():
    """Test interceptor fires on MCP projection via subprocess."""
    test_dir = os.path.dirname(os.path.abspath(__file__))
    src_dir = os.path.join(test_dir, "..", "src")
    gen_dir = os.path.join(test_dir, "proto", "gen")
    descriptor = os.path.join(test_dir, "proto", "descriptor.binpb")

    script = f"""
import asyncio
import sys
sys.path.insert(0, {src_dir!r})
sys.path.insert(0, {gen_dir!r})
import greet_pb2
import greet_pb2_grpc
import grpc
from invariant import Server

log = []

class SharedInterceptor(grpc.aio.ServerInterceptor):
    async def intercept_service(self, continuation, handler_call_details):
        handler = await continuation(handler_call_details)
        terminal = handler.unary_unary
        async def wrapped(request, context):
            log.append("A-before")
            response = await terminal(request, context)
            log.append("A-after")
            return response
        return grpc.unary_unary_rpc_method_handler(
            wrapped,
            request_deserializer=handler.request_deserializer,
            response_serializer=handler.response_serializer,
        )

class GreetServicer:
    async def Greet(self, request, context):
        return greet_pb2.GreetResponse(message=f"Hi {{request.name}}")
    async def GreetGroup(self, request, context):
        return greet_pb2.GreetGroupResponse(messages=[], count=0)
    async def StreamGreet(self, request, context):
        if False:
            yield greet_pb2.GreetResponse()

async def main():
    server = Server.from_descriptor({descriptor!r})
    server.exclude("*StreamGreet")
    greet_pb2_grpc.add_GreetServiceServicer_to_server(GreetServicer(), server)
    server.use(SharedInterceptor())
    await server.serve_projections(mcp=True)
    print(",".join(log), file=sys.stderr)

asyncio.run(main())
"""

    msg = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {
            "name": "GreetService.Greet",
            "arguments": {"name": "MCP"},
        },
    }
    stdin_data = json.dumps(msg) + "\n"

    proc = subprocess.run(
        [sys.executable, "-c", script],
        input=stdin_data,
        capture_output=True,
        text=True,
        timeout=10,
    )

    responses = []
    for line in proc.stdout.strip().split("\n"):
        if line.strip():
            responses.append(json.loads(line))

    assert len(responses) == 1
    content = responses[0]["result"]["content"]
    result = json.loads(content[0]["text"])
    assert result["message"] == "Hi MCP"

    assert "A-before,A-after" in proc.stderr
