"""Hardening: body-size limits, connect+proto streaming, stop() idempotency,
stream edge cases (empty stream, error before first chunk), oversized envelope.
"""

from __future__ import annotations

import json
import struct

import greet_pb2
import grpc
import httpx
import pytest
import pytest_asyncio
from conftest import DESCRIPTOR_PATH, register_greet

from invariant import ChannelOptions, InvariantError, MethodConfig, Server

_MCP_HEADERS = {
    "Accept": "application/json, text/event-stream",
    "MCP-Protocol-Version": "2025-11-25",
}

# -- HTTP unary body-size limit. --


def test_invariant_error_materializes_detail_generators_for_payload_and_trailers():
    from google.protobuf import duration_pb2
    from google.rpc import error_details_pb2

    def details():
        yield error_details_pb2.RetryInfo(retry_delay=duration_pb2.Duration(seconds=3))

    err = InvariantError(grpc.StatusCode.UNAVAILABLE, "retry later", details())

    assert err.to_payload()["details"] == [
        {
            "@type": "type.googleapis.com/google.rpc.RetryInfo",
            "retryDelay": "3s",
        }
    ]
    status = err.to_status_proto()
    assert len(status.details) == 1
    assert status.details[0].Is(error_details_pb2.RetryInfo.DESCRIPTOR)


def test_invariant_error_uses_connect_canceled_spelling():
    err = InvariantError(grpc.StatusCode.CANCELLED, "stopped")

    assert err.to_payload()["code"] == "canceled"


@pytest_asyncio.fixture
async def basic_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)

    class S:
        async def Greet(self, request, context):
            return greet_pb2.GreetResponse(message=f"hi {request.name}")

    register_greet(srv, S())
    yield srv
    await srv.stop()


async def test_http_unary_rejects_oversized_body(basic_server):
    from invariant.projections.http import HTTP_MAX_UNARY_REQUEST

    port = await basic_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            huge_name = "a" * (HTTP_MAX_UNARY_REQUEST + 1024)
            r = await client.post("/greet.v1.GreetService/Greet", json={"name": huge_name})
        assert r.status_code >= 400
        body = r.json()
        assert body["code"] == "resource_exhausted"
        assert "limit" in body["message"]
    finally:
        await basic_server._stop_http()


async def test_per_method_http_limits_cover_unary_and_each_stream_message():
    class LimitedServicer:
        async def Greet(self, request, context):
            return greet_pb2.GreetResponse(message="r" * 1024)

        async def StreamGreet(self, request, context):
            yield greet_pb2.GreetResponse(message="small")
            yield greet_pb2.GreetResponse(message="r" * 1024)

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.configure_method(
        "/greet.v1.GreetService/Greet",
        MethodConfig(max_unary_request_bytes=64, max_unary_response_bytes=256),
    )
    srv.configure_method(
        "/greet.v1.GreetService/StreamGreet",
        MethodConfig(max_stream_request_bytes=64, max_stream_response_bytes=64),
    )
    register_greet(srv, LimitedServicer())

    port = await srv._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            oversized_request = await client.post(
                "/greet.v1.GreetService/Greet",
                json={"name": "q" * 128},
            )
            oversized_response = await client.post(
                "/greet.v1.GreetService/Greet",
                json={"name": "ok"},
            )

            stream_request = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=_pack_envelope(0, b'{"name":"' + b"q" * 128 + b'"}'),
                headers={"Content-Type": "application/connect+json"},
            )
            stream_response = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=_pack_envelope(0, b'{"name":"ok"}'),
                headers={"Content-Type": "application/connect+json"},
            )

        assert oversized_request.json()["code"] == "resource_exhausted"
        assert oversized_response.json()["code"] == "resource_exhausted"

        request_frames = _read_frames(stream_request.content)
        assert len(request_frames) == 1
        assert json.loads(request_frames[0][1])["error"]["code"] == "resource_exhausted"

        response_frames = _read_frames(stream_response.content)
        assert len(response_frames) == 2
        first = json.loads(response_frames[0][1])
        assert first["message"] == "small"
        end = json.loads(response_frames[1][1])
        assert end["error"]["code"] == "resource_exhausted"
    finally:
        await srv._stop_http()
        await srv.stop()


# -- Connect+proto streaming. --


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


class StreamServicer:
    async def StreamGreet(self, request, context):
        n = request.count or 1
        for i in range(n):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #{i}")


@pytest_asyncio.fixture
async def stream_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, StreamServicer())
    yield srv
    await srv.stop()


async def test_http_connect_proto_streaming(stream_server):
    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            req = greet_pb2.StreamGreetRequest(name="Bin", count=3)
            body = _pack_envelope(0, req.SerializeToString())
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=body,
                headers={"Content-Type": "application/connect+proto"},
            )
        assert r.status_code == 200
        assert r.headers["content-type"] == "application/connect+proto"

        frames = _read_frames(r.content)
        assert len(frames) == 4
        for i in range(3):
            flags, payload = frames[i]
            assert flags == 0
            msg = greet_pb2.GreetResponse()
            msg.ParseFromString(payload)
            assert msg.message == f"Hi Bin #{i}"
        end_flags, end_payload = frames[3]
        assert end_flags & 0x02
        end = json.loads(end_payload)
        assert "error" not in end
    finally:
        await stream_server._stop_http()


# -- Stop / shutdown idempotency. --


async def test_stop_is_safe_to_call_twice():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)

    class S:
        async def Greet(self, request, context):
            return greet_pb2.GreetResponse(message=f"hi {request.name}")

    register_greet(srv, S())
    await srv.stop()
    # Calling stop again must not raise — keeps caller error handling simple.
    await srv.stop()


async def test_stop_is_safe_before_serve():
    """Construct a server, stop it without ever calling serve, no error."""
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    await srv.stop()


# -- Stream edge cases. --


class EmptyStreamServicer:
    async def StreamGreet(self, request, context):
        if False:  # pragma: no cover - silences async-gen detector
            yield greet_pb2.GreetResponse()


@pytest_asyncio.fixture
async def empty_stream_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, EmptyStreamServicer())
    yield srv
    await srv.stop()


async def test_empty_stream_produces_only_end_envelope(empty_stream_server):
    port = await empty_stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            body = _pack_envelope(0, b'{"name":"x"}')
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=body,
                headers={"Content-Type": "application/connect+json"},
            )
        frames = _read_frames(r.content)
        assert len(frames) == 1
        assert frames[0][0] & 0x02
    finally:
        await empty_stream_server._stop_http()


async def test_empty_stream_over_mcp_http(empty_stream_server):
    port = await empty_stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "tools/call",
                    "params": {"name": "GreetService.StreamGreet", "arguments": {"name": "x"}},
                },
                headers=_MCP_HEADERS,
            )
        result = r.json()["result"]
        assert "isError" not in result
        assert result["content"] == []
    finally:
        await empty_stream_server._stop_http()


class ImmediateErrServicer:
    async def StreamGreet(self, request, context):
        # An empty `if False: yield` makes this an async-gen function without
        # ever yielding — needed because async generators are detected by the
        # presence of `yield` syntax, not by runtime behaviour.
        if False:  # pragma: no cover
            yield greet_pb2.GreetResponse()
        raise InvariantError(grpc.StatusCode.FAILED_PRECONDITION, "nope")


@pytest_asyncio.fixture
async def immediate_err_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, ImmediateErrServicer())
    yield srv
    await srv.stop()


async def test_stream_error_before_any_chunk(immediate_err_server):
    port = await immediate_err_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            body = _pack_envelope(0, b'{"name":"x"}')
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=body,
                headers={"Content-Type": "application/connect+json"},
            )
        frames = _read_frames(r.content)
        assert len(frames) == 1
        assert frames[0][0] & 0x02
        end = json.loads(frames[0][1])
        assert end["error"]["code"] == "failed_precondition"
        assert "nope" in end["error"]["message"]
    finally:
        await immediate_err_server._stop_http()


# -- Connect envelope max-size guard. --


async def test_stream_rejects_oversized_request_envelope(stream_server):
    from invariant.projections.http import CONNECT_STREAM_MAX_REQUEST

    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            # Header advertises a length larger than CONNECT_STREAM_MAX_REQUEST.
            forged = bytes([0]) + struct.pack(">I", CONNECT_STREAM_MAX_REQUEST + 1)
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=forged,
                headers={"Content-Type": "application/connect+json"},
            )
        assert r.status_code == 200
        frames = _read_frames(r.content)
        assert len(frames) == 1
        assert frames[0][0] & 0x02
        body = json.loads(frames[0][1])["error"]
        assert body["code"] == "resource_exhausted"
    finally:
        await stream_server._stop_http()


# -- Connect-Timeout-Ms on streaming and MCP HTTP. --


class SlowStreamServicer:
    async def StreamGreet(self, request, context):
        import asyncio as _asyncio

        yield greet_pb2.GreetResponse(message="hi")
        await _asyncio.sleep(60)
        yield greet_pb2.GreetResponse(message="should-not-arrive")


@pytest_asyncio.fixture
async def slow_stream_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, SlowStreamServicer())
    yield srv
    await srv.stop()


async def test_streaming_connect_timeout_terminates_with_deadline_exceeded(slow_stream_server):
    port = await slow_stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            body = _pack_envelope(0, b'{"name":"X","count":2}')
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=body,
                headers={
                    "Content-Type": "application/connect+json",
                    "Connect-Timeout-Ms": "100",
                },
                timeout=5.0,
            )
        # Stream returns 200 then puts the deadline-exceeded error in the
        # end-stream envelope — that's the Connect-correct behaviour.
        assert r.status_code == 200
        frames = _read_frames(r.content)
        # Should have: 0..1 message frames + 1 end-stream with error.
        end_flags, end_payload = frames[-1]
        assert end_flags & 0x02
        end = json.loads(end_payload)
        assert end["error"]["code"] == "deadline_exceeded"
    finally:
        await slow_stream_server._stop_http()


class SlowUnaryServicer:
    async def Greet(self, request, context):
        import asyncio as _asyncio

        await _asyncio.sleep(60)
        return greet_pb2.GreetResponse(message=f"hi {request.name}")


@pytest_asyncio.fixture
async def slow_unary_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, SlowUnaryServicer())
    yield srv
    await srv.stop()


async def test_mcp_http_connect_timeout(slow_unary_server):
    """MCP HTTP must honor Connect-Timeout-Ms the same way unary Connect does."""
    port = await slow_unary_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post(
                "/mcp",
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "tools/call",
                    "params": {"name": "GreetService.Greet", "arguments": {"name": "x"}},
                },
                headers={**_MCP_HEADERS, "Connect-Timeout-Ms": "100"},
                timeout=5.0,
            )
        body = r.json()
        # The deadline error is wrapped in a Connect-style envelope at the
        # HTTP layer — clients see deadline_exceeded immediately rather than
        # waiting out the whole 60s sleep.
        assert body["code"] == "deadline_exceeded"
    finally:
        await slow_unary_server._stop_http()


# -- Oversized MCP HTTP request body. --


# -- Public invoke / invoke_stream type-mismatch errors. --


async def test_invoke_stream_delivers_chunks(stream_server):
    got = [
        msg.message
        async for msg in stream_server.invoke_stream(
            "GreetService.StreamGreet", greet_pb2.StreamGreetRequest(name="API", count=3)
        )
    ]
    assert got == ["Hi API #0", "Hi API #1", "Hi API #2"]


async def test_invoke_rejects_streaming_tool(stream_server):
    with pytest.raises(InvariantError) as exc:
        await stream_server.invoke("GreetService.StreamGreet", greet_pb2.StreamGreetRequest(name="x"))
    assert exc.value.code == grpc.StatusCode.FAILED_PRECONDITION
    assert "invoke_stream" in exc.value.message


async def test_invoke_stream_rejects_unary_tool(basic_server):
    async def consume():
        async for _ in basic_server.invoke_stream("GreetService.Greet", greet_pb2.GreetRequest(name="x")):
            pass

    with pytest.raises(InvariantError) as exc:
        await consume()
    assert exc.value.code == grpc.StatusCode.FAILED_PRECONDITION
    assert "invoke" in exc.value.message


async def test_invoke_stream_unknown_tool(stream_server):
    async def consume():
        async for _ in stream_server.invoke_stream("Nope.Nope", greet_pb2.GreetRequest()):
            pass

    with pytest.raises(InvariantError) as exc:
        await consume()
    assert exc.value.code == grpc.StatusCode.NOT_FOUND


# -- Outbound HTTP response size cap (connect_http proxy mode). --


async def test_connect_http_rejects_oversized_upstream_response():
    """A proxied REST upstream returning a huge body must not OOM the server."""
    import uvicorn

    max_receive = 1024

    # Tiny ASGI app: respond to anything with a giant JSON body.
    async def big_app(scope, receive, send):
        if scope["type"] != "http":
            if scope["type"] == "lifespan":
                while True:
                    m = await receive()
                    if m["type"] == "lifespan.startup":
                        await send({"type": "lifespan.startup.complete"})
                    elif m["type"] == "lifespan.shutdown":
                        await send({"type": "lifespan.shutdown.complete"})
                        return
            return
        # Drain request body
        while True:
            m = await receive()
            if not m.get("more_body", False):
                break
        filler = "x" * (max_receive + 1024)
        body = ('{"message":"' + filler + '"}').encode()
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [(b"content-type", b"application/json")],
            }
        )
        await send({"type": "http.response.body", "body": body})

    import asyncio as _asyncio

    config = uvicorn.Config(big_app, host="127.0.0.1", port=0, log_level="warning")
    upstream = uvicorn.Server(config)
    task = _asyncio.create_task(upstream.serve())
    try:
        # Wait for bind
        for _ in range(200):
            if upstream.started and upstream.servers:
                break
            await _asyncio.sleep(0.01)
        port = upstream.servers[0].sockets[0].getsockname()[1]

        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://127.0.0.1:{port}", options=ChannelOptions(max_receive_message_size=max_receive))
        try:
            with pytest.raises(InvariantError) as exc:
                await srv.invoke("GreetService.Greet", greet_pb2.GreetRequest(name="x"))
            assert exc.value.code == grpc.StatusCode.RESOURCE_EXHAUSTED
            assert "exceeds" in exc.value.message
        finally:
            await srv.stop()
    finally:
        upstream.should_exit = True
        await task


async def test_set_max_unary_request_bytes_lifts_cap(basic_server):
    """A server with the cap raised must accept a body that exceeds the default."""
    basic_server.set_max_unary_request_bytes(64 * 1024 * 1024)
    basic_server.set_max_unary_response_bytes(64 * 1024 * 1024)
    port = await basic_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            # 17 MiB — bigger than the default cap, smaller than the override.
            big_name = "a" * (17 * 1024 * 1024)
            r = await client.post(
                "/greet.v1.GreetService/Greet",
                json={"name": big_name},
            )
        assert r.status_code == 200
    finally:
        await basic_server._stop_http()


async def test_mcp_http_rejects_oversized_body(stream_server):
    from invariant.projections.http import HTTP_MAX_UNARY_REQUEST

    port = await stream_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            # Huge JSON-RPC payload; the read should fail well before parse.
            huge_args = {"name": "x" * (HTTP_MAX_UNARY_REQUEST + 1024)}
            r = await client.post(
                "/mcp",
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "tools/call",
                    "params": {"name": "GreetService.Greet", "arguments": huge_args},
                },
                headers=_MCP_HEADERS,
            )
        assert r.status_code >= 400
        body = r.json()
        assert body["code"] == "resource_exhausted"
    finally:
        await stream_server._stop_http()
