"""Production-grade behaviour: panic recovery (raises in handlers), /healthz,
multi-interceptor ordering, and stream cancellation propagation.
"""

from __future__ import annotations

import asyncio

import greet_pb2
import grpc
import httpx
import pytest
import pytest_asyncio
from conftest import DESCRIPTOR_PATH, register_greet

from invariant import InvariantError, Server

# -- Exception recovery: a handler raising RuntimeError must not crash the server. --


class RaisingServicer:
    async def Greet(self, request, context):
        raise RuntimeError("kaboom")

    async def StreamGreet(self, request, context):
        yield greet_pb2.GreetResponse(message="alive")
        raise RuntimeError("stream-kaboom")


@pytest_asyncio.fixture
async def raising_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, RaisingServicer())
    yield srv
    await srv.stop()


async def test_unary_handler_exception_propagates_as_invariant_error(raising_server):
    request = greet_pb2.GreetRequest(name="x")
    with pytest.raises(InvariantError, match="kaboom") as exc:
        await raising_server.invoke("greet.v1.GreetService.Greet", request)
    assert exc.value.code == grpc.StatusCode.INTERNAL
    assert "/greet.v1.GreetService/Greet" in exc.value.message


async def test_unary_exception_over_http_becomes_500(raising_server):
    port = await raising_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            r = await client.post("/greet.v1.GreetService/Greet", json={"name": "x"})
        assert r.status_code == 500
        body = r.json()
        assert body["code"] == "internal"
        assert "/greet.v1.GreetService/Greet" in body["message"]
        assert "kaboom" in body["message"]
    finally:
        await raising_server._stop_http()


async def test_stream_exception_over_http_lands_in_end_stream(raising_server):
    """A mid-stream raise should still produce a well-formed end-stream envelope
    rather than tearing the connection down or losing the prefix chunks.
    """
    import struct

    port = await raising_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            body = bytes([0]) + struct.pack(">I", len(b'{"name":"x"}')) + b'{"name":"x"}'
            r = await client.post(
                "/greet.v1.GreetService/StreamGreet",
                content=body,
                headers={"Content-Type": "application/connect+json"},
            )
        assert r.status_code == 200
        # Parse frames. We expect at least one message + one end-stream w/ error.
        data = r.content
        frames = []
        i = 0
        while i < len(data):
            flags = data[i]
            size = struct.unpack(">I", data[i + 1 : i + 5])[0]
            frames.append((flags, data[i + 5 : i + 5 + size]))
            i += 5 + size
        assert len(frames) >= 1
        end_flags, end_payload = frames[-1]
        assert end_flags & 0x02
        end = __import__("json").loads(end_payload)
        assert "error" in end
        assert "stream-kaboom" in end["error"]["message"]
    finally:
        await raising_server._stop_http()


# -- /healthz: every HTTP deployment needs a probe endpoint. --


@pytest_asyncio.fixture
async def basic_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)

    class S:
        async def Greet(self, request, context):
            return greet_pb2.GreetResponse(message=f"hi {request.name}")

    register_greet(srv, S())
    yield srv
    await srv.stop()


async def test_healthz_returns_ok(basic_server):
    port = await basic_server._start_http(port=0)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            for path in ("/healthz", "/readyz"):
                r = await client.get(path)
                assert r.status_code == 200, f"path={path}"
                assert r.json() == {"status": "ok"}, f"path={path}"
    finally:
        await basic_server._stop_http()


# -- Stream interceptor ordering: first registered = outermost. --


async def test_stream_interceptor_ordering():
    order: list[str] = []

    class S:
        async def StreamGreet(self, request, context):
            yield greet_pb2.GreetResponse(message="x")

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, S())

    class StreamInterceptor(grpc.aio.ServerInterceptor):
        def __init__(self, label):
            self.label = label

        async def intercept_service(self, continuation, handler_call_details):
            handler = await continuation(handler_call_details)
            assert handler is not None
            terminal = handler.unary_stream
            assert terminal is not None

            async def wrapped(request, context):
                order.append(f"{self.label}-before")
                async for msg in terminal(request, context):
                    yield msg
                order.append(f"{self.label}-after")

            return grpc.unary_stream_rpc_method_handler(
                wrapped,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

    srv.use(StreamInterceptor("outer"))
    srv.use(StreamInterceptor("inner"))

    msgs = [
        message
        async for message in srv.invoke_stream(
            "greet.v1.GreetService.StreamGreet", greet_pb2.StreamGreetRequest(name="x")
        )
    ]
    assert len(msgs) == 1
    assert order == ["outer-before", "inner-before", "inner-after", "outer-after"]
    await srv.stop()


# -- Stream cancellation: cancelling the consumer task stops the producer. --


async def test_stream_cancellation_stops_producer():
    started = asyncio.Event()
    finished = asyncio.Event()
    chunks_after_cancel = 0

    class S:
        async def StreamGreet(self, request, context):
            nonlocal chunks_after_cancel
            yield greet_pb2.GreetResponse(message="first")
            started.set()
            try:
                # Wait forever — only cancellation should release us.
                await asyncio.sleep(60)
                chunks_after_cancel += 1
                yield greet_pb2.GreetResponse(message="should-not-happen")
            finally:
                finished.set()

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, S())

    received = []

    async def consumer():
        async for msg in srv.invoke_stream("greet.v1.GreetService.StreamGreet", greet_pb2.StreamGreetRequest(name="x")):
            received.append(msg.message)

    task = asyncio.create_task(consumer())
    await asyncio.wait_for(started.wait(), timeout=2.0)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    await asyncio.wait_for(finished.wait(), timeout=2.0)
    assert received == ["first"]
    assert chunks_after_cancel == 0
    await srv.stop()


# -- Mid-stream InvariantError surfaces with its declared code. --


async def test_stream_invariant_error_preserves_code():
    class S:
        async def StreamGreet(self, request, context):
            yield greet_pb2.GreetResponse(message="x")
            raise InvariantError(grpc.StatusCode.RESOURCE_EXHAUSTED, "too many")

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, S())

    async def drain():
        out = []
        async for msg in srv.invoke_stream("greet.v1.GreetService.StreamGreet", greet_pb2.StreamGreetRequest(name="x")):
            out.append(msg.message)
        return out

    with pytest.raises(InvariantError) as exc:
        await drain()
    assert exc.value.code == grpc.StatusCode.RESOURCE_EXHAUSTED
    await srv.stop()
