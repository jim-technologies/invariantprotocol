"""HTTP projection — async ASGI app serving registered tools over Connect.

Routes:
  - POST /{package.Service}/{Method}      — invoke a tool (Connect protocol)
  - GET  /                                — tool catalog (same shape as MCP tools/list)
  - GET  /__invariant/tools               — tool catalog
  - GET  /__invariant/descriptor.binpb    — raw FileDescriptorSet bytes

Content types:
  - application/json (default)
  - application/proto (binary proto, Connect-standard)

Headers honored:
  - Connect-Timeout-Ms: server cancels the call after the requested deadline.

Errors are emitted in Connect format:
  {"code": "invalid_argument", "message": "...", "details": [...]}
"""

from __future__ import annotations

import asyncio
import json
from typing import TYPE_CHECKING, Any

import grpc
from google.protobuf import descriptor_pool, json_format, message_factory

from invariant.errors import (
    InvariantError,
    as_invariant_error,
    http_status_for,
    invalid_argument_from_json_error,
    not_found,
)

if TYPE_CHECKING:
    from invariant.server import Server

PROTO_CONTENT_TYPE = "application/proto"


def build_asgi_app(server: Server):
    """Build an ASGI application for the registered tools.

    Per-tool request/response classes are resolved here so each request just
    looks up the path → (tool, req_class) entry and instantiates the proto.
    """
    pool = descriptor_pool.Default()
    routes: dict[str, tuple[Any, Any]] = {}
    for t in server.tools.values():
        req_class = message_factory.GetMessageClass(pool.FindMessageTypeByName(t.input_type))
        routes[f"/{t.service_full_name}/{t.method_name}"] = (t, req_class)

    async def app(scope, receive, send):
        if scope["type"] != "http":
            if scope["type"] == "lifespan":
                while True:
                    msg = await receive()
                    if msg["type"] == "lifespan.startup":
                        await send({"type": "lifespan.startup.complete"})
                    elif msg["type"] == "lifespan.shutdown":
                        await send({"type": "lifespan.shutdown.complete"})
                        return
            return

        method = scope["method"]
        path = scope["path"]
        headers = _headers_dict(scope.get("headers", []))

        # Catalog endpoints.
        if method == "GET" and path in ("/", "/__invariant/tools"):
            await _send_tool_catalog(send, server)
            return
        if method == "GET" and path == "/__invariant/descriptor.binpb":
            await _send_descriptor(send, server)
            return

        route = routes.get(path)
        if route is None:
            await _send_error(send, not_found(f"Not found: {path}"))
            return
        if method != "POST":
            await _send_status(send, 405)
            return

        tool, req_class = route
        try:
            body_bytes = await _read_body(receive)
            request = req_class()

            if _is_proto(headers.get("content-type", "")):
                try:
                    request.ParseFromString(body_bytes)
                except Exception as e:
                    raise invalid_argument_from_json_error(e) from None
            else:
                if body_bytes.strip():
                    try:
                        json_format.Parse(body_bytes, request)
                    except Exception as e:
                        raise invalid_argument_from_json_error(e) from None

            timeout = _connect_timeout_seconds(headers)
            invoke_coro = server._invoke(tool, request, None)
            if timeout is not None:
                try:
                    response = await asyncio.wait_for(invoke_coro, timeout=timeout)
                except TimeoutError as e:
                    raise InvariantError(
                        grpc.StatusCode.DEADLINE_EXCEEDED,
                        f"deadline exceeded after {timeout * 1000:.0f}ms",
                    ) from e
            else:
                response = await invoke_coro

            if _is_proto(headers.get("accept", "")):
                payload = response.SerializeToString() if response is not None else b""
                await _send_bytes(send, 200, PROTO_CONTENT_TYPE, payload)
                return

            resp_dict = (
                json_format.MessageToDict(response, preserving_proto_field_name=True)
                if response is not None
                else {}
            )
            await _send_json(send, 200, resp_dict)
        except Exception as e:
            await _send_error(send, e)

    return app


def _headers_dict(raw_headers: list) -> dict[str, str]:
    out: dict[str, str] = {}
    for k, v in raw_headers:
        out[k.decode().lower()] = v.decode()
    return out


def _is_proto(value: str) -> bool:
    """Check whether a header value asks for binary proto."""
    if not value:
        return False
    for part in value.split(","):
        mt = part.split(";", 1)[0].strip().lower()
        if mt == PROTO_CONTENT_TYPE:
            return True
    return False


def _connect_timeout_seconds(headers: dict[str, str]) -> float | None:
    """Parse the Connect-Timeout-Ms header. Returns None if missing or invalid."""
    raw = headers.get("connect-timeout-ms")
    if not raw:
        return None
    try:
        ms = int(raw)
    except ValueError:
        return None
    if ms <= 0:
        return None
    return ms / 1000.0


async def _read_body(receive) -> bytes:
    chunks: list[bytes] = []
    more_body = True
    while more_body:
        msg = await receive()
        if msg["type"] == "http.disconnect":
            break
        chunks.append(msg.get("body", b""))
        more_body = msg.get("more_body", False)
    return b"".join(chunks)


async def _send_json(send, status: int, payload: dict[str, Any]) -> None:
    data = json.dumps(payload).encode()
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [
                (b"content-type", b"application/json"),
                (b"content-length", str(len(data)).encode()),
            ],
        }
    )
    await send({"type": "http.response.body", "body": data})


async def _send_status(send, status: int) -> None:
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [(b"content-length", b"0")],
        }
    )
    await send({"type": "http.response.body", "body": b""})


async def _send_bytes(send, status: int, content_type: str, payload: bytes) -> None:
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [
                (b"content-type", content_type.encode()),
                (b"content-length", str(len(payload)).encode()),
            ],
        }
    )
    await send({"type": "http.response.body", "body": payload})


async def _send_error(send, e: Exception) -> None:
    """Connect-style error envelope: {"code": "invalid_argument", "message": "...", ...}"""
    err = as_invariant_error(e)
    await _send_json(send, http_status_for(err.code), err.to_payload())


async def _send_tool_catalog(send, server: Server) -> None:
    await _send_json(send, 200, {"tools": server.tool_catalog()})


async def _send_descriptor(send, server: Server) -> None:
    if server._fds is None:
        await _send_status(send, 404)
        return
    await _send_bytes(send, 200, PROTO_CONTENT_TYPE, server._fds.SerializeToString())
