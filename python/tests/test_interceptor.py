"""Test interceptor middleware across all projections."""

import json
import os
import subprocess
import sys

import grpc
import httpx
import pytest
from google.protobuf import descriptor_pool, message_factory


async def test_interceptor_fires_on_cli(server):
    log = []

    async def interceptor(request, context, info, handler):
        log.append("A-before")
        resp = await handler(request, context)
        log.append("A-after")
        return resp

    server.use(interceptor)
    try:
        result = await server._cli(["GreetService", "Greet", "-r", '{"name": "CLI"}'])
        assert result["message"] == "Hi CLI"
        assert log == ["A-before", "A-after"]
    finally:
        server._interceptors.clear()


async def test_interceptor_fires_on_http(server):
    log = []

    async def interceptor(request, context, info, handler):
        log.append("A-before")
        resp = await handler(request, context)
        log.append("A-after")
        return resp

    server.use(interceptor)
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "HTTP"},
            )
        assert resp.json()["message"] == "Hi HTTP"
        assert log == ["A-before", "A-after"]
    finally:
        await server._stop_http()
        server._interceptors.clear()


async def test_interceptor_fires_on_grpc(server):
    log = []

    async def interceptor(request, context, info, handler):
        log.append("A-before")
        resp = await handler(request, context)
        log.append("A-after")
        return resp

    server.use(interceptor)
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
        server._interceptors.clear()


async def test_interceptor_chain_order(server):
    log = []

    def make_interceptor(label):
        async def interceptor(request, context, info, handler):
            log.append(f"{label}-before")
            resp = await handler(request, context)
            log.append(f"{label}-after")
            return resp

        return interceptor

    server.use(make_interceptor("A"))
    server.use(make_interceptor("B"))
    try:
        result = await server._cli(["GreetService", "Greet", "-r", '{"name": "Order"}'])
        assert result["message"] == "Hi Order"
        assert log == ["A-before", "B-before", "B-after", "A-after"]
    finally:
        server._interceptors.clear()


async def test_interceptor_short_circuit(server):
    async def blocking_interceptor(request, context, info, handler):
        raise ValueError("blocked by interceptor")

    server.use(blocking_interceptor)
    try:
        with pytest.raises(ValueError, match="blocked by interceptor"):
            await server._cli(["GreetService", "Greet", "-r", '{"name": "Blocked"}'])
    finally:
        server._interceptors.clear()


async def test_interceptor_full_method(server):
    captured = {}

    async def interceptor(request, context, info, handler):
        captured["full_method"] = info.full_method
        return await handler(request, context)

    server.use(interceptor)
    try:
        result = await server._cli(["GreetService", "Greet", "-r", '{"name": "Method"}'])
        assert result["message"] == "Hi Method"
        assert captured["full_method"] == "/greet.v1.GreetService/Greet"
    finally:
        server._interceptors.clear()


async def test_no_interceptors(server):
    assert len(server._interceptors) == 0
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "Compat"}'])
    assert result["message"] == "Hi Compat"


def test_interceptor_rejects_sync():
    from invariant import Server

    srv = Server.from_descriptor(os.path.join(os.path.dirname(os.path.abspath(__file__)), "proto", "descriptor.binpb"))

    def sync_interceptor(request, context, info, handler):
        return handler(request, context)

    with pytest.raises(TypeError, match="must be async"):
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
from invariant import Server

log = []

async def interceptor(request, context, info, handler):
    log.append("A-before")
    resp = await handler(request, context)
    log.append("A-after")
    return resp

class GreetServicer:
    async def Greet(self, request, context):
        return greet_pb2.GreetResponse(message=f"Hi {{request.name}}")
    async def GreetGroup(self, request, context):
        return greet_pb2.GreetGroupResponse(messages=[], count=0)

async def main():
    server = Server.from_descriptor({descriptor!r})
    server.register(GreetServicer())
    server.use(interceptor)
    await server.serve(mcp=True)
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
