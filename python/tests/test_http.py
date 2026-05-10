"""Test HTTP projection (ASGI, Connect-only)."""

import httpx


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
        assert body["details"][0]["fieldViolations"][0]["field"] == "extra"
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

    from invariant import Server

    descriptor_path = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")

    class SlowServicer:
        async def Greet(self, request, context):
            await _asyncio.sleep(2.0)
            return greet_pb2.GreetResponse(message=f"Hi {request.name}")

        async def GreetGroup(self, request, context):
            return greet_pb2.GreetGroupResponse()

    srv = Server.from_descriptor(descriptor_path)
    srv.register(SlowServicer())
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
