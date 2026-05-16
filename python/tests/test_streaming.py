"""Tests for server-streaming RPCs across all projections.

The conftest's GreetServicer covers only the unary methods so that existing
tests still see exactly 2 tools. These tests register a separate servicer that
adds StreamGreet so we can exercise the streaming path end-to-end.
"""

from __future__ import annotations

import json
import struct

import greet_pb2
import grpc
import httpx
import pytest
import pytest_asyncio
from conftest import DESCRIPTOR_PATH
from google.protobuf import descriptor_pool, message_factory

from invariant import InvariantError, Server


class StreamGreetServicer:
    """Combined unary + stream servicer used by streaming tests."""

    async def Greet(self, request, context):
        return greet_pb2.GreetResponse(message=f"Hi {request.name}")

    async def GreetGroup(self, request, context):
        return greet_pb2.GreetGroupResponse(
            messages=[f"Hi {p.name}" for p in request.people],
            count=len(request.people),
        )

    async def StreamGreet(self, request, context):
        n = request.count or 1
        for i in range(n):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #{i}")


class StreamErrorServicer:
    async def StreamGreet(self, request, context):
        yield greet_pb2.GreetResponse(message=f"first {request.name}")
        raise InvariantError(grpc.StatusCode.FAILED_PRECONDITION, "kapow")


@pytest_asyncio.fixture
async def stream_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(StreamGreetServicer())
    yield srv
    await srv.stop()


@pytest_asyncio.fixture
async def stream_err_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(StreamErrorServicer())
    yield srv
    await srv.stop()


# -- Registration / direct dispatch --


def test_register_streaming_marks_tool_as_streaming(stream_server):
    tool = stream_server.tools["GreetService.StreamGreet"]
    assert tool.server_streaming is True


def test_tool_catalog_marks_streaming_tools(stream_server):
    catalog = stream_server.tool_catalog()
    by_name = {entry["name"]: entry for entry in catalog}

    stream = by_name["GreetService.StreamGreet"]
    assert stream["_meta"] == {"streaming": True}

    # Unary tools intentionally have no _meta so the wire shape stays compact.
    unary = by_name["GreetService.Greet"]
    assert "_meta" not in unary


def test_register_rejects_non_async_gen_stream_handler():
    class BadServicer:
        async def StreamGreet(self, request, context):
            # async def without yield — coroutine, not async gen.
            return greet_pb2.GreetResponse(message="nope")

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(TypeError, match="async generator"):
        srv.register(BadServicer())


async def test_invoke_stream_collects_chunks(stream_server):
    tool = stream_server.tools["GreetService.StreamGreet"]
    request = greet_pb2.StreamGreetRequest(name="Alice", count=3)
    msgs = [m.message async for m in stream_server._invoke_stream(tool, request, None)]
    assert msgs == ["Hi Alice #0", "Hi Alice #1", "Hi Alice #2"]


async def test_stream_interceptor_chain(stream_server):
    seen = []

    async def trace(request, context, info, handler):
        seen.append(info.full_method)
        async for msg in handler(request, context):
            seen.append(msg.message)
            yield msg

    stream_server.use_stream(trace)

    tool = stream_server.tools["GreetService.StreamGreet"]
    request = greet_pb2.StreamGreetRequest(name="Z", count=2)
    out = [m async for m in stream_server._invoke_stream(tool, request, None)]
    assert len(out) == 2
    assert seen[0] == "/greet.v1.GreetService/StreamGreet"
    assert "Hi Z #0" in seen
    assert "Hi Z #1" in seen


def test_use_stream_rejects_non_async_gen(stream_server):
    async def not_a_generator(request, context, info, handler):  # coroutine, not async gen
        return None

    with pytest.raises(TypeError, match="async generator"):
        stream_server.use_stream(not_a_generator)


# -- MCP projection (in-process dispatch) --


async def test_mcp_tools_call_collects_stream_chunks(stream_server):
    from invariant.projections.mcp import mcp_dispatch

    msg = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {
            "name": "GreetService.StreamGreet",
            "arguments": {"name": "Stream", "count": 3},
        },
    }
    resp = await mcp_dispatch(stream_server, msg)
    result = resp["result"]
    assert "isError" not in result
    content = result["content"]
    assert len(content) == 3
    for i, block in enumerate(content):
        assert block["type"] == "text"
        chunk = json.loads(block["text"])
        assert chunk["message"] == f"Hi Stream #{i}"


async def test_mcp_tools_call_stream_error_surfaces(stream_err_server):
    from invariant.projections.mcp import mcp_dispatch

    msg = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": "GreetService.StreamGreet", "arguments": {"name": "x"}},
    }
    resp = await mcp_dispatch(stream_err_server, msg)
    result = resp["result"]
    assert result["isError"] is True
    assert result["error"]["code"] == "failed_precondition"
    # The first chunk should still appear before the error text block.
    assert len(result["content"]) >= 2
    assert "kapow" in result["content"][-1]["text"]


# -- CLI projection --


async def test_cli_streams_ndjson(stream_server):
    from invariant.projections.cli import run_cli

    out = await run_cli(stream_server, ["GreetService", "StreamGreet", "-r", '{"name":"Z","count":2}'])
    assert isinstance(out, str)
    lines = out.split("\n")
    assert len(lines) == 2
    parsed = [json.loads(line) for line in lines]
    assert parsed[0]["message"] == "Hi Z #0"
    assert parsed[1]["message"] == "Hi Z #1"


async def test_stream_cli_flushes_per_chunk():
    """stream_cli must invoke ``write`` once per chunk as it arrives. We use
    an asyncio.Event-gated servicer to prove chunk 1 is written before chunk
    2 has been produced — buffered output would deadlock here.
    """
    import asyncio

    from invariant.projections.cli import stream_cli

    gate = asyncio.Event()

    class GatedServicer:
        async def StreamGreet(self, request, context):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #0")
            await gate.wait()
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #1")

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GatedServicer())

    written: list[str] = []
    flushed_first = asyncio.Event()

    def write(piece: str) -> None:
        written.append(piece)
        if len(written) == 1:
            flushed_first.set()

    runner = asyncio.create_task(
        stream_cli(srv, ["GreetService", "StreamGreet", "-r", '{"name":"X","count":2}'], write)
    )

    await asyncio.wait_for(flushed_first.wait(), timeout=2.0)
    assert len(written) == 1
    chunk0 = json.loads(written[0])
    assert chunk0["message"] == "Hi X #0"

    gate.set()
    await asyncio.wait_for(runner, timeout=2.0)
    assert len(written) == 2
    chunk1 = json.loads(written[1])
    assert chunk1["message"] == "Hi X #1"
    await srv.stop()


# -- gRPC projection --


def _stream_stub(channel, method: str, resp_type: str):
    pool = descriptor_pool.Default()
    resp_class = message_factory.GetMessageClass(pool.FindMessageTypeByName(resp_type))
    return channel.unary_stream(
        method,
        request_serializer=lambda m: m.SerializeToString(),
        response_deserializer=resp_class.FromString,
    )


async def test_grpc_server_streaming(stream_server):
    port = await stream_server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = _stream_stub(channel, "/greet.v1.GreetService/StreamGreet", "greet.v1.GreetResponse")
            stream = stub(greet_pb2.StreamGreetRequest(name="Gee", count=3))
            msgs = [resp.message async for resp in stream]
        assert msgs == ["Hi Gee #0", "Hi Gee #1", "Hi Gee #2"]
    finally:
        await stream_server._stop_grpc()


async def test_grpc_stream_error_becomes_status(stream_err_server):
    port = await stream_err_server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = _stream_stub(channel, "/greet.v1.GreetService/StreamGreet", "greet.v1.GreetResponse")
            stream = stub(greet_pb2.StreamGreetRequest(name="X"))
            with pytest.raises(grpc.aio.AioRpcError) as exc_info:
                async for _ in stream:
                    pass
            assert exc_info.value.code() == grpc.StatusCode.FAILED_PRECONDITION
            assert "kapow" in exc_info.value.details()
    finally:
        await stream_err_server._stop_grpc()


# -- HTTP / Connect projection --


def _pack_envelope(flags: int, payload: bytes) -> bytes:
    return bytes([flags & 0xFF]) + struct.pack(">I", len(payload)) + payload


def _read_frames(data: bytes) -> list[tuple[int, bytes]]:
    out = []
    i = 0
    while i < len(data):
        flags = data[i]
        size = struct.unpack(">I", data[i + 1 : i + 5])[0]
        payload = data[i + 5 : i + 5 + size]
        out.append((flags, payload))
        i += 5 + size
        if flags & 0x02:
            break
    return out


async def test_http_connect_stream_envelopes(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            body = _pack_envelope(0, json.dumps({"name": "K", "count": 3}).encode())
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=body,
                headers={"Content-Type": "application/connect+json"},
            )
        assert r.status_code == 200
        assert r.headers["content-type"] == "application/connect+json"

        frames = _read_frames(r.content)
        assert len(frames) == 4  # 3 messages + end-stream
        for i in range(3):
            flags, payload = frames[i]
            assert flags == 0
            chunk = json.loads(payload)
            assert chunk["message"] == f"Hi K #{i}"
        end_flags, end_payload = frames[3]
        assert end_flags & 0x02
        end = json.loads(end_payload)
        assert "error" not in end
    finally:
        await stream_server._stop_http()


async def test_http_connect_stream_rejects_wrong_content_type(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                json={"name": "K", "count": 1},
            )
        assert r.status_code == 400
        body = r.json()
        assert body["code"] == "invalid_argument"
        assert "application/connect+json" in body["message"]
    finally:
        await stream_server._stop_http()


async def test_http_connect_stream_error_in_end_envelope(stream_err_server):
    port = await stream_err_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            body = _pack_envelope(0, json.dumps({"name": "K"}).encode())
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=body,
                headers={"Content-Type": "application/connect+json"},
            )
        assert r.status_code == 200
        frames = _read_frames(r.content)
        assert len(frames) >= 1
        end_flags, end_payload = frames[-1]
        assert end_flags & 0x02
        end = json.loads(end_payload)
        assert end["error"]["code"] == "failed_precondition"
        assert "kapow" in end["error"]["message"]
    finally:
        await stream_err_server._stop_http()


# -- MCP HTTP transport --


async def test_mcp_http_initialize(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 1, "method": "initialize"},
            )
        assert r.status_code == 200
        body = r.json()
        assert body["result"]["protocolVersion"] == "2024-11-05"
        assert body["result"]["serverInfo"]["name"] == "invariant-protocol"
    finally:
        await stream_server._stop_http()


async def test_mcp_http_tools_list_includes_stream(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
            )
        names = [t["name"] for t in r.json()["result"]["tools"]]
        assert set(names) == {"GreetService.Greet", "GreetService.GreetGroup", "GreetService.StreamGreet"}
    finally:
        await stream_server._stop_http()


async def test_mcp_http_unary_tools_call(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={
                    "jsonrpc": "2.0",
                    "id": 3,
                    "method": "tools/call",
                    "params": {"name": "GreetService.Greet", "arguments": {"name": "World"}},
                },
            )
        result = r.json()["result"]
        text = json.loads(result["content"][0]["text"])
        assert text["message"] == "Hi World"
    finally:
        await stream_server._stop_http()


async def test_mcp_http_stream_tools_call_collects_chunks(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={
                    "jsonrpc": "2.0",
                    "id": 4,
                    "method": "tools/call",
                    "params": {
                        "name": "GreetService.StreamGreet",
                        "arguments": {"name": "Stream", "count": 3},
                    },
                },
            )
        result = r.json()["result"]
        assert len(result["content"]) == 3
    finally:
        await stream_server._stop_http()


async def test_mcp_http_notification_returns_204(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "method": "notifications/initialized"},
            )
        assert r.status_code == 204
    finally:
        await stream_server._stop_http()


async def test_mcp_http_parse_error(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                content="{not json",
                headers={"Content-Type": "application/json"},
            )
        body = r.json()
        assert body["error"]["code"] == -32700
    finally:
        await stream_server._stop_http()


async def test_mcp_http_unknown_method(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 5, "method": "does/not/exist"},
            )
        assert r.json()["error"]["code"] == -32601
    finally:
        await stream_server._stop_http()


# Also assert that adding /mcp didn't break the existing tool POST surface.
async def test_mcp_http_does_not_shadow_tool_endpoint(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/greet.v1.GreetService/Greet",
                json={"name": "World"},
            )
        assert r.status_code == 200
        assert r.json()["message"] == "Hi World"
    finally:
        await stream_server._stop_http()
