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
CONNECT_STREAM_JSON = "application/connect+json"
CONNECT_STREAM_PROTO = "application/connect+proto"
CONNECT_END_STREAM_FLAG = 0x02
CONNECT_STREAM_MAX_REQUEST = 16 * 1024 * 1024  # 16 MiB safety cap on streaming request envelopes
HTTP_MAX_UNARY_REQUEST = 16 * 1024 * 1024  # 16 MiB safety cap on unary request bodies


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
        if method == "GET" and path in ("/healthz", "/readyz"):
            await _send_json(send, 200, {"status": "ok"})
            return
        if method == "GET" and path == "/__invariant/descriptor.binpb":
            await _send_descriptor(send, server)
            return

        # MCP Streamable HTTP transport.
        if method == "POST" and path == "/mcp":
            await _serve_mcp_http(send, receive, server, headers)
            return

        route = routes.get(path)
        if route is None:
            await _send_error(send, not_found(f"Not found: {path}"))
            return
        if method != "POST":
            await _send_status(send, 405)
            return

        tool, req_class = route

        # Server-streaming tools speak Connect's streaming protocol (envelope
        # frames). Distinct content type from unary, intentional split.
        if tool.server_streaming:
            await _serve_stream(send, receive, server, tool, req_class, headers)
            return

        try:
            body_bytes = await _read_body(receive, max_bytes=server._http_max_unary_request)
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
                json_format.MessageToDict(response, preserving_proto_field_name=True) if response is not None else {}
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


async def _read_body(receive, max_bytes: int = HTTP_MAX_UNARY_REQUEST) -> bytes:
    """Read the full request body, raising InvariantError if it exceeds max_bytes.

    A hostile or buggy client could otherwise stream an unbounded body and
    exhaust the server's memory.
    """
    chunks: list[bytes] = []
    total = 0
    more_body = True
    while more_body:
        msg = await receive()
        if msg["type"] == "http.disconnect":
            break
        chunk = msg.get("body", b"")
        total += len(chunk)
        if total > max_bytes:
            raise InvariantError(
                grpc.StatusCode.RESOURCE_EXHAUSTED,
                f"request body exceeds {max_bytes} byte limit",
            )
        chunks.append(chunk)
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


def _is_connect_stream_json(value: str) -> bool:
    return _match_content_type(value, CONNECT_STREAM_JSON)


def _is_connect_stream_proto(value: str) -> bool:
    return _match_content_type(value, CONNECT_STREAM_PROTO)


def _match_content_type(value: str, want: str) -> bool:
    if not value:
        return False
    return value.split(";", 1)[0].strip().lower() == want


def _pack_envelope(flags: int, payload: bytes) -> bytes:
    """Encode one Connect envelope frame: [flags:1B][len:uint32 BE][data]."""
    size = len(payload)
    return bytes([flags & 0xFF, (size >> 24) & 0xFF, (size >> 16) & 0xFF, (size >> 8) & 0xFF, size & 0xFF]) + payload


def _unpack_envelope(data: bytes, max_size: int = CONNECT_STREAM_MAX_REQUEST) -> tuple[int, bytes]:
    """Decode one envelope from data. Raises if too short or oversized."""
    if len(data) < 5:
        raise InvariantError(
            grpc.StatusCode.INVALID_ARGUMENT,
            "stream request body shorter than envelope header",
        )
    flags = data[0]
    size = (data[1] << 24) | (data[2] << 16) | (data[3] << 8) | data[4]
    if size > max_size:
        raise InvariantError(
            grpc.StatusCode.INVALID_ARGUMENT,
            f"envelope size {size} exceeds max {max_size}",
        )
    if len(data) < 5 + size:
        raise InvariantError(
            grpc.StatusCode.INVALID_ARGUMENT,
            "stream request body truncated",
        )
    return flags, data[5 : 5 + size]


async def _serve_stream(send, receive, server: Server, tool, req_class, headers: dict[str, str]) -> None:
    """Serve a server-streaming RPC over Connect's streaming wire format.

    Request body: a single envelope wrapping the request (JSON or binary proto).
    Response body: zero or more message envelopes, then one end-stream envelope
    (flags=0x02) carrying either ``{}`` (success) or ``{"error": {...}}`` (failure).
    The end-stream payload is always JSON regardless of the message content type.
    """
    ct = headers.get("content-type", "")
    binary = _is_connect_stream_proto(ct)
    if not binary and not _is_connect_stream_json(ct):
        await _send_error(
            send,
            InvariantError(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"streaming tools require Content-Type: {CONNECT_STREAM_JSON} or {CONNECT_STREAM_PROTO}",
            ),
        )
        return

    try:
        body = await _read_body(receive, max_bytes=server._connect_stream_max_request)
    except InvariantError as e:
        await _send_error(send, e)
        return

    request = req_class()
    try:
        _, payload = _unpack_envelope(body, max_size=server._connect_stream_max_request)
        if binary:
            if payload:
                request.ParseFromString(payload)
        elif payload.strip():
            json_format.Parse(payload, request)
    except InvariantError as e:
        await _send_error(send, e)
        return
    except Exception as e:
        await _send_error(send, invalid_argument_from_json_error(e))
        return

    resp_ct = CONNECT_STREAM_PROTO if binary else CONNECT_STREAM_JSON
    await send(
        {
            "type": "http.response.start",
            "status": 200,
            "headers": [(b"content-type", resp_ct.encode())],
        }
    )

    end_payload = b"{}"
    timeout = _connect_timeout_seconds(headers)
    try:
        if timeout is not None:
            async with asyncio.timeout(timeout):
                end_payload = await _drain_stream(server, tool, request, binary, send)
        else:
            end_payload = await _drain_stream(server, tool, request, binary, send)
    except TimeoutError:
        err = InvariantError(
            grpc.StatusCode.DEADLINE_EXCEEDED,
            f"deadline exceeded after {timeout * 1000:.0f}ms",
        )
        end_payload = json.dumps({"error": err.to_payload()}).encode()
    except Exception as e:
        err = as_invariant_error(e)
        end_payload = json.dumps({"error": err.to_payload()}).encode()

    await send(
        {
            "type": "http.response.body",
            "body": _pack_envelope(CONNECT_END_STREAM_FLAG, end_payload),
            "more_body": False,
        }
    )


async def _drain_stream(server: Server, tool, request, binary: bool, send) -> bytes:
    """Iterate the stream handler, packing each message into an envelope.
    Returns the success end-stream payload (`b"{}"`) when iteration completes.
    Splitting this out lets the timeout / exception path stay readable.
    """
    async for msg in server._invoke_stream(tool, request, None):
        if binary:
            payload = msg.SerializeToString()
        else:
            payload = json_format.MessageToJson(msg, preserving_proto_field_name=True, indent=None).encode()
        await send(
            {
                "type": "http.response.body",
                "body": _pack_envelope(0, payload),
                "more_body": True,
            }
        )
    return b"{}"


async def _serve_mcp_http(send, receive, server: Server, headers: dict[str, str]) -> None:
    """Serve one MCP JSON-RPC request over POST /mcp.

    Reads one JSON-RPC envelope, dispatches through the shared mcp_dispatch,
    and returns the JSON-RPC response. Notifications (no id) get a 204.
    """
    from invariant.projections.mcp import mcp_dispatch  # local import to avoid cycle on module load

    try:
        body = await _read_body(receive, max_bytes=server._http_max_unary_request)
    except InvariantError as e:
        await _send_error(send, e)
        return

    try:
        msg = json.loads(body)
    except json.JSONDecodeError as e:
        await _send_json(
            send,
            200,
            {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": f"Parse error: {e}"}},
        )
        return

    timeout = _connect_timeout_seconds(headers)
    try:
        if timeout is not None:
            async with asyncio.timeout(timeout):
                response = await mcp_dispatch(server, msg)
        else:
            response = await mcp_dispatch(server, msg)
    except TimeoutError:
        err = InvariantError(
            grpc.StatusCode.DEADLINE_EXCEEDED,
            f"deadline exceeded after {timeout * 1000:.0f}ms",
        )
        await _send_error(send, err)
        return
    except Exception as e:
        await _send_error(send, e)
        return

    if response is None:
        await _send_status(send, 204)
        return
    await _send_json(send, 200, response)
