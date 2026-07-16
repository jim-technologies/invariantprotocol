"""Tests for server-streaming RPCs across all projections.

The conftest's GreetServicer covers only the unary methods so that existing
tests still see exactly 2 tools. These tests register a separate servicer that
adds StreamGreet so we can exercise the streaming path end-to-end.
"""

from __future__ import annotations

import json
import struct

import greet_pb2
import greet_pb2_grpc
import grpc
import httpx
import pytest
import pytest_asyncio
from conftest import DESCRIPTOR_PATH, register_greet
from google.protobuf import descriptor_pool, message_factory

from invariant import InvariantError, Server

_MCP_ACCEPT = {"Accept": "application/json, text/event-stream"}
_MCP_HEADERS = {**_MCP_ACCEPT, "MCP-Protocol-Version": "2025-11-25"}
_MCP_INITIALIZE = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
        "protocolVersion": "2025-11-25",
        "capabilities": {},
        "clientInfo": {"name": "invariant-test", "version": "1.0"},
    },
}


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
    register_greet(srv, StreamGreetServicer())
    yield srv
    await srv.stop()


@pytest_asyncio.fixture
async def stream_err_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, StreamErrorServicer())
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
    class BadServicer(StreamGreetServicer):
        async def StreamGreet(self, request, context):
            # async def without yield — coroutine, not async gen.
            return greet_pb2.GreetResponse(message="nope")

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(TypeError, match="async generator"):
        greet_pb2_grpc.add_GreetServiceServicer_to_server(BadServicer(), srv)


async def test_invoke_stream_collects_chunks(stream_server):
    tool = stream_server.tools["GreetService.StreamGreet"]
    request = greet_pb2.StreamGreetRequest(name="Alice", count=3)
    msgs = [m.message async for m in stream_server._invoke_stream(tool, request, None)]
    assert msgs == ["Hi Alice #0", "Hi Alice #1", "Hi Alice #2"]


async def test_stream_interceptor_chain(stream_server):
    seen = []

    class Trace(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            handler = await continuation(handler_call_details)
            assert handler is not None
            terminal = handler.unary_stream
            assert terminal is not None

            async def wrapped(request, context):
                seen.append((handler_call_details.method, type(request)))
                async for msg in terminal(request, context):
                    seen.append(msg.message)
                    yield msg

            return grpc.unary_stream_rpc_method_handler(
                wrapped,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

    stream_server.use(Trace())

    tool = stream_server.tools["GreetService.StreamGreet"]
    request = greet_pb2.StreamGreetRequest(name="Z", count=2)
    out = [m async for m in stream_server._invoke_stream(tool, request, None)]
    assert len(out) == 2
    assert seen[0] == ("/greet.v1.GreetService/StreamGreet", greet_pb2.StreamGreetRequest)
    assert "Hi Z #0" in seen
    assert "Hi Z #1" in seen


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

    seen = []

    class Trace(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            handler = await continuation(handler_call_details)
            assert handler is not None
            terminal = handler.unary_stream
            assert terminal is not None

            async def wrapped(request, context):
                seen.append((handler_call_details.method, type(request)))
                async for message in terminal(request, context):
                    yield message

            return grpc.unary_stream_rpc_method_handler(
                wrapped,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

    stream_server.use(Trace())
    out = await run_cli(stream_server, ["GreetService", "StreamGreet", "-r", '{"name":"Z","count":2}'])
    assert isinstance(out, str)
    lines = out.split("\n")
    assert len(lines) == 2
    parsed = [json.loads(line) for line in lines]
    assert parsed[0]["message"] == "Hi Z #0"
    assert parsed[1]["message"] == "Hi Z #1"
    assert seen == [("/greet.v1.GreetService/StreamGreet", greet_pb2.StreamGreetRequest)]


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
    register_greet(srv, GatedServicer())

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
            stub = greet_pb2_grpc.GreetServiceStub(channel)
            stream = stub.StreamGreet(greet_pb2.StreamGreetRequest(name="Gee", count=3))
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
            responses = [
                await client.post(
                    "/greet.v1.GreetService/StreamGreet",
                    content=b'{"name":"K","count":1}',
                ),
                await client.post(
                    "/greet.v1.GreetService/StreamGreet",
                    content=b'{"name":"K","count":1}',
                    headers={"Content-Type": "text/plain"},
                ),
            ]
        for response in responses:
            assert response.status_code == 415
            assert response.headers["content-type"] == "application/json"
            assert response.json()["code"] == "invalid_argument"
            assert "application/connect+json" in response.json()["message"]
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
                json=_MCP_INITIALIZE,
                headers=_MCP_ACCEPT,
            )
            negotiated = await client.post(
                "/mcp",
                json={
                    **_MCP_INITIALIZE,
                    "id": 2,
                    "params": {**_MCP_INITIALIZE["params"], "protocolVersion": "2099-01-01"},
                },
                headers=_MCP_ACCEPT,
            )
        assert r.status_code == 200
        body = r.json()
        assert body["result"]["protocolVersion"] == "2025-11-25"
        assert body["result"]["serverInfo"]["name"] == "invariant-protocol"
        assert negotiated.status_code == 200
        assert negotiated.json()["result"]["protocolVersion"] == "2025-11-25"
    finally:
        await stream_server._stop_http()


async def test_mcp_http_tools_list_includes_stream(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
                headers=_MCP_HEADERS,
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
                headers=_MCP_HEADERS,
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
                headers=_MCP_HEADERS,
            )
        result = r.json()["result"]
        assert len(result["content"]) == 3
    finally:
        await stream_server._stop_http()


async def test_mcp_http_notification_and_client_response_return_202(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "method": "notifications/initialized"},
                headers=_MCP_HEADERS,
            )
            client_response = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 7, "result": {}},
                headers=_MCP_HEADERS,
            )
            error_response = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "error": {"code": -32000, "message": "ignored"}},
                headers=_MCP_HEADERS,
            )
        assert r.status_code == 202
        assert r.content == b""
        assert client_response.status_code == 202
        assert client_response.content == b""
        assert error_response.status_code == 202
        assert error_response.content == b""
    finally:
        await stream_server._stop_http()


async def test_mcp_http_parse_error(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                content="{not json",
                headers={"Content-Type": "application/json", **_MCP_HEADERS},
            )
        body = r.json()
        assert body["error"]["code"] == -32700
    finally:
        await stream_server._stop_http()


async def test_mcp_http_rejects_nonfinite_json_and_invalid_client_response(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            nonfinite = await client.post(
                "/mcp",
                content='{"jsonrpc":"2.0","id":1,"method":"ping","params":{"value":NaN}}',
                headers={**_MCP_HEADERS, "Content-Type": "application/json"},
            )
            invalid_utf8 = await client.post(
                "/mcp",
                content=b"\xff",
                headers={**_MCP_HEADERS, "Content-Type": "application/json"},
            )
            invalid_response = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": None, "result": {}},
                headers=_MCP_HEADERS,
            )
            boolean_error_code = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "error": {"code": True, "message": "invalid"}},
                headers=_MCP_HEADERS,
            )

        assert nonfinite.status_code == 200
        assert nonfinite.json()["error"]["code"] == -32700
        assert invalid_utf8.status_code == 200
        assert invalid_utf8.json()["error"]["code"] == -32700
        assert invalid_response.status_code == 200
        assert invalid_response.json()["error"]["code"] == -32600
        assert boolean_error_code.status_code == 200
        assert boolean_error_code.json()["error"]["code"] == -32600
    finally:
        await stream_server._stop_http()


async def test_mcp_http_invalid_request_shape(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            for request in (
                {"jsonrpc": "1.0", "id": 1, "method": "ping"},
                {"jsonrpc": "2.0", "id": 1.0, "method": "ping"},
                {"jsonrpc": "2.0", "id": 9007199254740992, "method": "ping"},
            ):
                response = await client.post("/mcp", json=request, headers=_MCP_HEADERS)
                assert response.status_code == 200
                assert response.json() == {
                    "jsonrpc": "2.0",
                    "id": None,
                    "error": {"code": -32600, "message": "Invalid Request"},
                }
    finally:
        await stream_server._stop_http()


async def test_mcp_http_invalid_method_params(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            response = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": []},
                headers=_MCP_HEADERS,
            )
        assert response.status_code == 200
        assert response.json() == {
            "jsonrpc": "2.0",
            "id": 2,
            "error": {"code": -32602, "message": "Invalid params"},
        }
    finally:
        await stream_server._stop_http()


async def test_mcp_http_unknown_method(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 5, "method": "does/not/exist"},
                headers=_MCP_HEADERS,
            )
        assert r.json()["error"]["code"] == -32601
    finally:
        await stream_server._stop_http()


async def test_mcp_http_response_limit(stream_server):
    stream_server.set_max_unary_response_bytes(160)
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            response = await client.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 8, "method": "tools/list"},
                headers=_MCP_HEADERS,
            )
        assert response.status_code == 429
        assert response.json()["code"] == "resource_exhausted"
    finally:
        await stream_server._stop_http()


async def test_mcp_http_transport_headers_and_get(stream_server):
    port = await stream_server._start_http(port=0)
    initialize = _MCP_INITIALIZE
    tools_list = {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            for accept in ("", "application/json", "text/event-stream", "application/json, text/event-stream;q=0"):
                response = await client.post("/mcp", json=initialize, headers={"Accept": accept})
                assert response.status_code == 406

            origin = await client.post(
                "/mcp",
                json=initialize,
                headers={**_MCP_ACCEPT, "Origin": "https://example.test"},
            )
            assert origin.status_code == 403
            for method in ("GET", "DELETE"):
                hostile = await client.request(
                    method,
                    "/mcp",
                    headers={"Origin": "https://example.test"},
                )
                assert hostile.status_code == 403

            unsupported_content_type = await client.post(
                "/mcp",
                content=b"{}",
                headers={**_MCP_HEADERS, "Content-Type": "text/plain"},
            )
            assert unsupported_content_type.status_code == 415

            for protocol_version in (None, "2099-01-01"):
                headers = dict(_MCP_ACCEPT)
                if protocol_version is not None:
                    headers["MCP-Protocol-Version"] = protocol_version
                response = await client.post("/mcp", json=tools_list, headers=headers)
                assert response.status_code == 400

            unsupported_initialize = await client.post(
                "/mcp",
                json=initialize,
                headers={**_MCP_ACCEPT, "MCP-Protocol-Version": "2099-01-01"},
            )
            assert unsupported_initialize.status_code == 400

            get_response = await client.get("/mcp")
            assert get_response.status_code == 405
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
