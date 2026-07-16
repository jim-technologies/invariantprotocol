"""Test HTTP projection (ASGI, Connect-only)."""

import asyncio
import base64
import json
import time

import greet_pb2
import httpx
import pytest
from conftest import DESCRIPTOR_PATH, register_greet
from google.rpc import error_details_pb2

from invariant import Server


async def _call_asgi(app, path: str, headers: dict[str, str], receive) -> list[dict]:
    sent: list[dict] = []

    async def send(message):
        sent.append(message)

    await asyncio.wait_for(
        app(
            {
                "type": "http",
                "method": "POST",
                "path": path,
                "headers": [(key.encode(), value.encode()) for key, value in headers.items()],
                "client": ("127.0.0.1", 12345),
            },
            receive,
            send,
        ),
        timeout=2,
    )
    return sent


def _asgi_body(messages: list[dict]) -> bytes:
    return b"".join(message.get("body", b"") for message in messages if message["type"] == "http.response.body")


async def test_greet_http(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "World"},
            )
        assert resp.status_code == 200
        assert resp.json()["message"] == "Hi World"
    finally:
        await server._stop_http()


async def test_greet_http_different_name(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "Claude"},
            )
        assert resp.json()["message"] == "Hi Claude"
    finally:
        await server._stop_http()


async def test_greet_http_not_found(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/DoesNotExist",
                json={},
            )
        assert resp.status_code == 404
    finally:
        await server._stop_http()


async def test_greet_http_with_enum_and_tags(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={
                    "name": "World",
                    "mood": "MOOD_HAPPY",
                    "tags": {"lang": "en", "source": "test"},
                },
            )
        body = resp.json()
        assert body["message"] == "Hi World"
        assert body["mood"] == "MOOD_HAPPY"
        assert body["tags"]["lang"] == "en"
        assert body["tags"]["source"] == "test"
    finally:
        await server._stop_http()


async def test_greet_group_http(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/GreetGroup",
                json={
                    "people": [
                        {"name": "Alice", "mood": "MOOD_HAPPY"},
                        {"name": "Bob"},
                    ]
                },
            )
        body = resp.json()
        assert body["messages"] == ["Hi Alice", "Hi Bob"]
        assert body["count"] == 2
    finally:
        await server._stop_http()


async def test_greet_group_http_empty(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/GreetGroup",
                json={"people": []},
            )
        body = resp.json()
        assert body.get("messages", []) == []
        assert body.get("count", 0) == 0
    finally:
        await server._stop_http()


async def test_greet_http_method_not_allowed(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.get(f"http://localhost:{port}/greet.v1.GreetService/Greet")
        assert resp.status_code in (405, 501)
    finally:
        await server._stop_http()


async def test_greet_http_invalid_json(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                content=b"not valid json",
                headers={"Content-Type": "application/json"},
            )
        assert resp.status_code == 400
        assert resp.json()["code"] == "invalid_argument"
    finally:
        await server._stop_http()


async def test_greet_http_rejects_missing_and_unsupported_content_type(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            responses = [
                await client.post(
                    f"http://localhost:{port}/greet.v1.GreetService/Greet",
                    content=b'{"name":"World"}',
                ),
                await client.post(
                    f"http://localhost:{port}/greet.v1.GreetService/Greet",
                    content=b'{"name":"World"}',
                    headers={"Content-Type": "text/plain"},
                ),
            ]
        for response in responses:
            assert response.status_code == 415
            assert response.headers["content-type"] == "application/json"
            assert response.json()["code"] == "invalid_argument"
            assert "application/json" in response.json()["message"]
    finally:
        await server._stop_http()


async def test_greet_http_unknown_field_rejected(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "World", "extra": "x"},
            )
        assert resp.status_code == 400
        body = resp.json()
        assert body["code"] == "invalid_argument"
        assert 'field named "extra"' in body["message"]
        detail = body["details"][0]
        assert set(detail) == {"type", "value"}
        assert detail["type"] == "google.rpc.BadRequest"
        value = base64.b64decode(detail["value"] + "=" * (-len(detail["value"]) % 4))
        bad_request = error_details_pb2.BadRequest.FromString(value)
        assert bad_request.field_violations[0].field == "extra"
    finally:
        await server._stop_http()


async def test_tool_catalog(server):
    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.get(f"http://localhost:{port}/")
        body = resp.json()
        names = {t["name"] for t in body["tools"]}
        assert names == {"GreetService.Greet", "GreetService.GreetGroup"}
    finally:
        await server._stop_http()


async def test_greet_http_binary_proto(server):
    """POST with application/proto body, accept binary response."""
    import greet_pb2

    port = await server._start_http(port=0)
    try:
        req = greet_pb2.GreetRequest(name="Binary")
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                content=req.SerializeToString(),
                headers={
                    "Content-Type": "application/proto",
                    "Accept": "application/proto",
                },
            )
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "application/proto"

        out = greet_pb2.GreetResponse()
        out.ParseFromString(resp.content)
        assert out.message == "Hi Binary"
    finally:
        await server._stop_http()


async def test_connect_timeout_ms_honored():
    """A small Connect-Timeout-Ms causes a slow handler to return DEADLINE_EXCEEDED."""
    import asyncio as _asyncio
    import os

    import greet_pb2
    from conftest import register_greet

    from invariant import Server

    descriptor_path = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")
    context_was_cancelled: list[bool] = []

    class SlowServicer:
        async def Greet(self, request, context):
            try:
                await _asyncio.sleep(2.0)
                return greet_pb2.GreetResponse(message=f"Hi {request.name}")
            finally:
                context_was_cancelled.append(context.cancelled())

        async def GreetGroup(self, request, context):
            return greet_pb2.GreetGroupResponse()

    srv = Server.from_descriptor(descriptor_path)
    register_greet(srv, SlowServicer())
    port = await srv._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "World"},
                headers={"Connect-Timeout-Ms": "50"},
            )
        assert resp.status_code == 504  # DEADLINE_EXCEEDED → HTTP 504
        body = resp.json()
        assert body["code"] == "deadline_exceeded"
        assert context_was_cancelled == [True]
    finally:
        await srv._stop_http()
        await srv.stop()


async def test_connect_timeout_ms_rejects_zero_and_malformed_values_across_projections():
    called = False

    class TrackingServicer:
        async def Greet(self, request, context):
            nonlocal called
            del request, context
            called = True
            return greet_pb2.GreetResponse()

        async def StreamGreet(self, request, context):
            nonlocal called
            del request, context
            called = True
            yield greet_pb2.GreetResponse()

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, TrackingServicer())
    app = srv.asgi_app()

    async def receive():
        raise AssertionError("malformed timeout headers must be rejected before reading the body")

    try:
        for value in ("", "0", "+10", "-1", " 1", "1 ", "1e3", "1.5", "12345678901", "\uff11\uff12"):
            response = await _call_asgi(
                app,
                "/greet.v1.GreetService/Greet",
                {"content-type": "application/json", "connect-timeout-ms": value},
                receive,
            )
            assert next(message["status"] for message in response if message["type"] == "http.response.start") == 400
            assert json.loads(_asgi_body(response))["code"] == "invalid_argument"

        for path, headers in (
            (
                "/greet.v1.GreetService/StreamGreet",
                {"content-type": "application/connect+json", "connect-timeout-ms": "0"},
            ),
            (
                "/mcp",
                {
                    "content-type": "application/json",
                    "accept": "application/json, text/event-stream",
                    "mcp-protocol-version": "2025-11-25",
                    "connect-timeout-ms": "0",
                },
            ),
        ):
            response = await _call_asgi(app, path, headers, receive)
            assert next(message["status"] for message in response if message["type"] == "http.response.start") == 400
            assert json.loads(_asgi_body(response))["code"] == "invalid_argument"
        assert not called
    finally:
        await srv.stop()


async def test_connect_timeout_catches_non_yielding_handler_after_it_returns():
    called = False

    class BlockingServicer:
        async def Greet(self, request, context):
            nonlocal called
            del context
            called = True
            time.sleep(0.1)
            return greet_pb2.GreetResponse(message=request.name)

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, BlockingServicer())
    app = srv.asgi_app()
    incoming: asyncio.Queue[dict] = asyncio.Queue()
    await incoming.put({"type": "http.request", "body": b'{"name":"late"}', "more_body": False})

    async def receive():
        return await incoming.get()

    try:
        sent = await _call_asgi(
            app,
            "/greet.v1.GreetService/Greet",
            {"content-type": "application/json", "connect-timeout-ms": "50"},
            receive,
        )
        assert called
        assert next(message["status"] for message in sent if message["type"] == "http.response.start") == 504
        assert json.loads(_asgi_body(sent))["code"] == "deadline_exceeded"
    finally:
        await srv.stop()


async def test_connect_timeout_starts_before_unary_body_read():
    called = False

    class TrackingServicer:
        async def Greet(self, request, context):
            nonlocal called
            del request, context
            called = True
            return greet_pb2.GreetResponse()

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, TrackingServicer())
    app = srv.asgi_app()

    async def receive():
        await asyncio.Event().wait()

    try:
        sent = await _call_asgi(
            app,
            "/greet.v1.GreetService/Greet",
            {"content-type": "application/json", "connect-timeout-ms": "10"},
            receive,
        )
        assert not called
        assert next(message["status"] for message in sent if message["type"] == "http.response.start") == 504
        assert json.loads(_asgi_body(sent))["code"] == "deadline_exceeded"
    finally:
        await srv.stop()


async def test_connect_timeout_starts_before_stream_and_mcp_body_reads():
    called = False

    class TrackingServicer:
        async def Greet(self, request, context):
            nonlocal called
            del request, context
            called = True
            return greet_pb2.GreetResponse()

        async def StreamGreet(self, request, context):
            nonlocal called
            del request, context
            called = True
            yield greet_pb2.GreetResponse()

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, TrackingServicer())
    app = srv.asgi_app()

    async def receive():
        await asyncio.Event().wait()

    try:
        stream = await _call_asgi(
            app,
            "/greet.v1.GreetService/StreamGreet",
            {
                "content-type": "application/connect+json",
                "connect-timeout-ms": "10",
            },
            receive,
        )
        assert next(message["status"] for message in stream if message["type"] == "http.response.start") == 200
        stream_payload = _asgi_body(stream)
        assert stream_payload[0] & 0x02
        assert json.loads(stream_payload[5:])["error"]["code"] == "deadline_exceeded"

        mcp = await _call_asgi(
            app,
            "/mcp",
            {
                "content-type": "application/json",
                "accept": "application/json, text/event-stream",
                "mcp-protocol-version": "2025-11-25",
                "connect-timeout-ms": "10",
            },
            receive,
        )
        assert next(message["status"] for message in mcp if message["type"] == "http.response.start") == 504
        assert json.loads(_asgi_body(mcp))["code"] == "deadline_exceeded"
        assert not called
    finally:
        await srv.stop()


async def test_stream_and_mcp_metadata_mapper_errors_are_bounded():
    def mapper(headers):
        del headers
        raise RuntimeError("mapper failed")

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.use_http_metadata_mapper(mapper)
    register_greet(srv, object())
    app = srv.asgi_app()

    async def receive():
        raise AssertionError("mapper failures must be contained before reading the body")

    try:
        stream = await _call_asgi(
            app,
            "/greet.v1.GreetService/StreamGreet",
            {"content-type": "application/connect+json"},
            receive,
        )
        assert next(message["status"] for message in stream if message["type"] == "http.response.start") == 200
        stream_payload = _asgi_body(stream)
        assert stream_payload[0] & 0x02
        assert json.loads(stream_payload[5:])["error"]["code"] == "unknown"

        mcp = await _call_asgi(
            app,
            "/mcp",
            {
                "content-type": "application/json",
                "accept": "application/json, text/event-stream",
                "mcp-protocol-version": "2025-11-25",
            },
            receive,
        )
        assert next(message["status"] for message in mcp if message["type"] == "http.response.start") == 500
        assert json.loads(_asgi_body(mcp))["code"] == "unknown"
    finally:
        await srv.stop()


async def test_http_disconnect_wins_when_application_and_disconnect_finish_together():
    from invariant.projection_context import ProjectionContext
    from invariant.projections.http import _HTTPDisconnectedError, _run_until_disconnect

    ready = asyncio.Event()
    ready.set()
    context = ProjectionContext(peer="invariant:test")

    async def work():
        await ready.wait()
        return "late response"

    async def receive():
        await ready.wait()
        return {"type": "http.disconnect"}

    with pytest.raises(_HTTPDisconnectedError):
        await _run_until_disconnect(work(), receive, context)
    assert context.cancelled()


async def test_http_disconnect_cancels_unary_handler():
    started = asyncio.Event()
    cancelled = asyncio.Event()
    context_cancelled: list[bool] = []

    class BlockingServicer:
        async def Greet(self, request, context):
            del request
            started.set()
            try:
                await asyncio.Event().wait()
            finally:
                context_cancelled.append(context.cancelled())
                cancelled.set()

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, BlockingServicer())
    app = srv.asgi_app()
    incoming: asyncio.Queue[dict] = asyncio.Queue()
    await incoming.put(
        {
            "type": "http.request",
            "body": b'{"name":"World"}',
            "more_body": False,
        }
    )
    sent: list[dict] = []

    async def receive():
        return await incoming.get()

    async def send(message):
        sent.append(message)

    request = asyncio.create_task(
        app(
            {
                "type": "http",
                "method": "POST",
                "path": "/greet.v1.GreetService/Greet",
                "headers": [(b"content-type", b"application/json")],
                "client": ("127.0.0.1", 12345),
            },
            receive,
            send,
        )
    )
    try:
        await asyncio.wait_for(started.wait(), timeout=2)
        await incoming.put({"type": "http.disconnect"})
        await asyncio.wait_for(request, timeout=2)
        await asyncio.wait_for(cancelled.wait(), timeout=2)
        assert context_cancelled == [True]
        assert not any(message["type"] == "http.response.start" for message in sent)
    finally:
        if not request.done():
            request.cancel()
            await asyncio.gather(request, return_exceptions=True)
        await srv.stop()


async def test_http_asgi_ignores_invalid_header_names_and_decodes_values_as_latin1():
    observed: dict[str, str] = {}
    decoded: dict[str, str] = {}

    class HeaderServicer:
        async def Greet(self, request, context):
            observed.update(dict(context.invocation_metadata()))
            return greet_pb2.GreetResponse(message=request.name)

    def mapper(headers):
        assert all(key.isascii() for key in headers)
        decoded["x-custom"] = headers["x-custom"]
        return (("x-custom", headers["x-custom"]),)

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.use_http_metadata_mapper(mapper)
    register_greet(srv, HeaderServicer())
    app = srv.asgi_app()
    incoming: asyncio.Queue[dict] = asyncio.Queue()
    await incoming.put(
        {
            "type": "http.request",
            "body": b'{"name":"World"}',
            "more_body": False,
        }
    )
    sent: list[dict] = []

    async def receive():
        return await incoming.get()

    async def send(message):
        sent.append(message)

    await app(
        {
            "type": "http",
            "method": "POST",
            "path": "/greet.v1.GreetService/Greet",
            "headers": [
                (b"content-type", b"application/json"),
                (b"\xff-invalid", b"ignored"),
                (b"bad header", b"ignored"),
                (b"x-custom", b"\xff"),
            ],
            "client": ("127.0.0.1", 12345),
        },
        receive,
        send,
    )

    assert decoded == {"x-custom": "ÿ"}
    assert observed == {}
    start = next(message for message in sent if message["type"] == "http.response.start")
    body = next(message["body"] for message in sent if message["type"] == "http.response.body")
    assert start["status"] == 200
    assert json.loads(body)["message"] == "World"
    await srv.stop()


async def test_http_disconnect_cancels_server_streaming_handler():
    waiting = asyncio.Event()
    cancelled = asyncio.Event()
    context_cancelled: list[bool] = []

    class BlockingStreamServicer:
        async def StreamGreet(self, request, context):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name}")
            waiting.set()
            try:
                await asyncio.Event().wait()
            finally:
                context_cancelled.append(context.cancelled())
                cancelled.set()

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, BlockingStreamServicer())
    app = srv.asgi_app()
    payload = b'{"name":"World"}'
    envelope = bytes([0]) + len(payload).to_bytes(4, "big") + payload
    incoming: asyncio.Queue[dict] = asyncio.Queue()
    await incoming.put(
        {
            "type": "http.request",
            "body": envelope,
            "more_body": False,
        }
    )
    sent: list[dict] = []

    async def receive():
        return await incoming.get()

    async def send(message):
        sent.append(message)

    request = asyncio.create_task(
        app(
            {
                "type": "http",
                "method": "POST",
                "path": "/greet.v1.GreetService/StreamGreet",
                "headers": [(b"content-type", b"application/connect+json")],
                "client": ("127.0.0.1", 12345),
            },
            receive,
            send,
        )
    )
    try:
        await asyncio.wait_for(waiting.wait(), timeout=2)
        await incoming.put({"type": "http.disconnect"})
        await asyncio.wait_for(request, timeout=2)
        await asyncio.wait_for(cancelled.wait(), timeout=2)
        assert context_cancelled == [True]
        assert any(message["type"] == "http.response.start" for message in sent)
        assert any(message["type"] == "http.response.body" and message.get("more_body") is True for message in sent)
        assert not any(
            message["type"] == "http.response.body" and message.get("more_body") is False for message in sent
        )
    finally:
        if not request.done():
            request.cancel()
            await asyncio.gather(request, return_exceptions=True)
        await srv.stop()


async def test_http_projection_supplies_grpc_context_metadata_and_status():
    import os

    import grpc
    from conftest import register_greet

    from invariant import Server

    descriptor_path = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")
    observed = {}

    class ContextServicer:
        async def Greet(self, request, context):
            observed["metadata"] = dict(context.invocation_metadata())
            observed["peer"] = context.peer()
            observed["time_remaining"] = context.time_remaining()
            await context.send_initial_metadata(
                (
                    ("x-projection-header", "leading"),
                    ("content-type", "must-not-override-connect"),
                )
            )
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "bad request",
                (("x-projection-trailer", "trailing"),),
            )

    srv = Server.from_descriptor(descriptor_path)
    register_greet(srv, ContextServicer())
    port = await srv._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "World"},
                headers={
                    "Authorization": "Bearer untrusted",
                    "Connect-Timeout-Ms": "1000",
                    "X-Request-Id": "request-123",
                },
            )

        assert resp.status_code == 400
        assert resp.json()["code"] == "invalid_argument"
        assert resp.json()["message"] == "bad request"
        assert observed["metadata"] == {"x-request-id": "request-123"}
        assert observed["peer"].startswith("invariant:http:")
        assert 0 < observed["time_remaining"] <= 1
        assert resp.headers["content-type"] == "application/json"
        assert resp.headers["x-projection-header"] == "leading"
        assert resp.headers["trailer-x-projection-trailer"] == "trailing"
    finally:
        await srv._stop_http()
        await srv.stop()


async def test_custom_http_metadata_mapper_remains_safe_and_freezes_with_server():
    observed = {}

    class ContextServicer:
        async def Greet(self, request, context):
            observed.update(dict(context.invocation_metadata()))
            return greet_pb2.GreetResponse(message=request.name)

    def mapper(headers):
        return (
            ("x-custom", headers["x-custom"]),
            ("trace-bin", headers["x-trace-bin"]),
            ("authorization", headers["authorization"]),
            ("x-tenant-id", headers["x-tenant-id"]),
            ("invalid key", "ignored"),
            ("authorization-bin", headers["x-trace-bin"]),
            ("proxy-authorization-bin", headers["x-trace-bin"]),
            ("authentication-bin", headers["x-trace-bin"]),
            ("api-key-bin", headers["x-trace-bin"]),
            ("x-api-key-bin", headers["x-trace-bin"]),
            ("cookie-bin", headers["x-trace-bin"]),
            ("set-cookie-bin", headers["x-trace-bin"]),
            ("identity-bin", headers["x-trace-bin"]),
        )

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.use_http_metadata_mapper(mapper)
    register_greet(srv, ContextServicer())
    port = await srv._start_http(port=0)
    try:
        binary_value = base64.b64encode(b"\x01\x02").decode().rstrip("=")
        async with httpx.AsyncClient() as client:
            response = await client.post(
                f"http://localhost:{port}/greet.v1.GreetService/Greet",
                json={"name": "mapped"},
                headers={
                    "X-Custom": "forwarded",
                    "X-Trace-Bin": binary_value,
                    "Authorization": "Bearer untrusted",
                    "X-Tenant-Id": "untrusted-tenant",
                },
            )

        assert response.status_code == 200
        assert observed == {"x-custom": "forwarded", "trace-bin": b"\x01\x02"}
        with pytest.raises(RuntimeError, match="cannot be changed"):
            srv.use_http_metadata_mapper(None)
    finally:
        await srv._stop_http()
        await srv.stop()


async def test_descriptor_endpoint(server):
    """GET /__invariant/descriptor.binpb returns the FileDescriptorSet."""
    from google.protobuf import descriptor_pb2

    port = await server._start_http(port=0)
    try:
        async with httpx.AsyncClient() as client:
            resp = await client.get(f"http://localhost:{port}/__invariant/descriptor.binpb")
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "application/proto"

        fds = descriptor_pb2.FileDescriptorSet()
        fds.ParseFromString(resp.content)
        # at least the greet proto file should be present
        names = {f.name for f in fds.file}
        assert any("greet" in n for n in names)
    finally:
        await server._stop_http()
