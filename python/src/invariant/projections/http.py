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
import base64
import json
import time
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

import grpc
from google.protobuf import json_format

from invariant.errors import (
    InvariantError,
    as_invariant_error,
    http_status_for,
    invalid_argument_from_json_error,
    not_found,
)
from invariant.projection_context import Metadata, ProjectionContext

if TYPE_CHECKING:
    from invariant.server import Server

PROTO_CONTENT_TYPE = "application/proto"
CONNECT_STREAM_JSON = "application/connect+json"
CONNECT_STREAM_PROTO = "application/connect+proto"
CONNECT_END_STREAM_FLAG = 0x02
CONNECT_STREAM_MAX_REQUEST = 16 * 1024 * 1024  # 16 MiB safety cap on streaming request envelopes
CONNECT_STREAM_MAX_RESPONSE = 16 * 1024 * 1024
HTTP_MAX_UNARY_REQUEST = 16 * 1024 * 1024  # 16 MiB safety cap on unary request bodies
HTTP_MAX_UNARY_RESPONSE = 16 * 1024 * 1024
CONNECT_CONTROL_MAX = 1024 * 1024
SequenceHeader = tuple[tuple[bytes, bytes], ...]


@dataclass(frozen=True, slots=True)
class _HTTPRoute:
    tool: Any
    max_unary_request: int
    max_unary_response: int
    max_stream_request: int
    max_stream_response: int


def build_asgi_app(server: Server):
    """Build an ASGI application for the registered tools.

    Per-tool generated request factories were captured at service registration,
    so request dispatch does not depend on the process-global descriptor pool.
    """
    server._freeze()
    routes: dict[str, _HTTPRoute] = {}
    for tool in server._tools.values():
        routes[f"/{tool.service_full_name}/{tool.method_name}"] = _HTTPRoute(
            tool=tool,
            max_unary_request=server._method_limit(tool, "max_unary_request_bytes", server._http_max_unary_request),
            max_unary_response=server._method_limit(tool, "max_unary_response_bytes", server._http_max_unary_response),
            max_stream_request=server._method_limit(
                tool, "max_stream_request_bytes", server._connect_stream_max_request
            ),
            max_stream_response=server._method_limit(
                tool, "max_stream_response_bytes", server._connect_stream_max_response
            ),
        )

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
            await _serve_mcp_http(send, receive, server, headers, scope)
            return
        if path == "/mcp":
            # The optional SSE receive stream is intentionally not offered.
            await _send_status(send, 405)
            return

        route = routes.get(path)
        if route is None:
            await _send_error(send, not_found(f"Not found: {path}"))
            return
        if method != "POST":
            await _send_status(send, 405)
            return

        tool = route.tool

        # Server-streaming tools speak Connect's streaming protocol (envelope
        # frames). Distinct content type from unary, intentional split.
        if tool.server_streaming:
            await _serve_stream(send, receive, server, route, headers, scope)
            return

        projection_context: ProjectionContext | None = None
        try:
            content_type = headers.get("content-type", "")
            json_request = _match_content_type(content_type, "application/json")
            proto_request = _is_proto(content_type)
            if not json_request and not proto_request:
                raise InvariantError(
                    grpc.StatusCode.INVALID_ARGUMENT,
                    "unary tools require Content-Type: application/json or application/proto",
                )
            content_encoding = headers.get("content-encoding", "").strip().lower()
            if content_encoding not in ("", "identity"):
                raise InvariantError(
                    grpc.StatusCode.UNIMPLEMENTED,
                    f"Content-Encoding {content_encoding!r} is not supported",
                )

            body_bytes = await _read_body(receive, max_bytes=route.max_unary_request)
            request = tool.new_request()

            if proto_request:
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
            projection_context = _http_projection_context(server, scope, headers, timeout)
            if timeout is not None:
                try:
                    async with asyncio.timeout(timeout):
                        response = await server._invoke(tool, request, projection_context)
                except TimeoutError as e:
                    projection_context.finish(cancelled=True)
                    raise InvariantError(
                        grpc.StatusCode.DEADLINE_EXCEEDED,
                        f"deadline exceeded after {timeout * 1000:.0f}ms",
                    ) from e
            else:
                response = await server._invoke(tool, request, projection_context)

            if _is_proto(headers.get("accept", "")):
                payload = response.SerializeToString() if response is not None else b""
                await _send_bytes(
                    send,
                    200,
                    PROTO_CONTENT_TYPE,
                    payload,
                    extra_headers=_context_response_headers(projection_context),
                    max_bytes=route.max_unary_response,
                )
                return

            resp_dict = (
                json_format.MessageToDict(response, preserving_proto_field_name=True) if response is not None else {}
            )
            await _send_json(
                send,
                200,
                resp_dict,
                extra_headers=_context_response_headers(projection_context),
                max_bytes=route.max_unary_response,
            )
        except Exception as e:
            await _send_error(
                send,
                e,
                extra_headers=_context_response_headers(projection_context) if projection_context is not None else (),
                max_bytes=route.max_unary_response,
            )
        finally:
            if projection_context is not None:
                projection_context.finish(cancelled=projection_context.cancelled())

    return app


def _headers_dict(raw_headers: list) -> dict[str, str]:
    out: dict[str, str] = {}
    for k, v in raw_headers:
        out[k.decode().lower()] = v.decode()
    return out


def default_http_metadata_mapper(headers: Mapping[str, str]) -> Sequence[tuple[str, str | bytes]]:
    """Forward only common tracing and correlation headers by default."""
    return tuple(
        (key, headers[key]) for key in ("traceparent", "tracestate", "baggage", "x-request-id") if key in headers
    )


def _inbound_http_metadata(server: Server, headers: Mapping[str, str]) -> Metadata:
    mapped = server._http_metadata_mapper(headers)
    out: list[tuple[str, str | bytes]] = []
    for raw_key, raw_value in mapped:
        key = str(raw_key).strip().lower()
        if not _valid_metadata_key(key) or _reserved_inbound_metadata(key):
            continue
        if key.endswith("-bin"):
            if isinstance(raw_value, bytes):
                out.append((key, raw_value))
                continue
            if isinstance(raw_value, str):
                try:
                    padding = "=" * (-len(raw_value) % 4)
                    out.append((key, base64.b64decode(raw_value + padding, validate=True)))
                except ValueError:
                    continue
            continue
        if isinstance(raw_value, str) and _valid_ascii_metadata_value(raw_value):
            out.append((key, raw_value))
    return tuple(out)


def _reserved_inbound_metadata(key: str) -> bool:
    if key.startswith(
        (
            "grpc-",
            "connect-",
            "invariant-internal-",
            "x-invariant-internal-",
            "x-tenant",
            "x-principal",
            "x-role",
            "x-user",
            "x-auth",
            "x-internal-",
            "internal-",
            "tenant-",
            "principal-",
            "role-",
            "user-",
            "auth-",
            "subject-",
            "identity-",
        )
    ):
        return True
    return key in {
        "authorization",
        "proxy-authorization",
        "cookie",
        "set-cookie",
        "authentication",
        "api-key",
        "x-api-key",
        "tenant",
        "principal",
        "role",
        "user",
        "subject",
        "identity",
        "te",
        "host",
        "connection",
        "keep-alive",
        "proxy-connection",
        "transfer-encoding",
        "upgrade",
        "content-length",
        "content-type",
        "trailer",
    }


def _http_projection_context(
    server: Server,
    scope: dict[str, Any],
    headers: dict[str, str],
    timeout: float | None,
) -> ProjectionContext:
    metadata = _inbound_http_metadata(server, headers)
    client = scope.get("client")
    peer = f"invariant:http:{client[0]}:{client[1]}" if client else "invariant:http"
    deadline = time.monotonic() + timeout if timeout is not None else None
    return ProjectionContext(peer=peer, invocation_metadata=metadata, deadline=deadline)


def _context_response_headers(context: ProjectionContext) -> SequenceHeader:
    return (
        *_encode_response_metadata(context.initial_metadata()),
        *_encode_response_metadata(context.trailing_metadata(), trailer=True),
    )


def _encode_response_metadata(metadata: Metadata, *, trailer: bool = False) -> SequenceHeader:
    encoded: list[tuple[bytes, bytes]] = []
    for key, value in metadata:
        key = key.lower()
        if _reserved_response_metadata(key) or not _valid_metadata_key(key):
            continue
        name = f"trailer-{key}" if trailer else key
        if key.endswith("-bin"):
            if not isinstance(value, bytes):
                continue
            wire_value = base64.b64encode(value).rstrip(b"=")
        else:
            if not isinstance(value, str) or not _valid_ascii_metadata_value(value):
                continue
            wire_value = value.encode("ascii")
        encoded.append((name.encode("ascii"), wire_value))
    return tuple(encoded)


def _connect_end_metadata(metadata: Metadata) -> dict[str, list[str]]:
    encoded: dict[str, list[str]] = {}
    for key, value in metadata:
        key = key.lower()
        if _reserved_response_metadata(key) or not _valid_metadata_key(key):
            continue
        if key.endswith("-bin"):
            if not isinstance(value, bytes):
                continue
            wire_value = base64.b64encode(value).rstrip(b"=").decode("ascii")
        else:
            if not isinstance(value, str) or not _valid_ascii_metadata_value(value):
                continue
            wire_value = value
        encoded.setdefault(key, []).append(wire_value)
    return encoded


def _reserved_response_metadata(key: str) -> bool:
    if key.startswith(("grpc-", "connect-", "trailer-", "invariant-internal-", "x-invariant-internal-")):
        return True
    return key in {
        "content-length",
        "content-type",
        "content-encoding",
        "transfer-encoding",
        "accept-encoding",
        "connection",
        "keep-alive",
        "proxy-connection",
        "te",
        "trailer",
        "upgrade",
        "host",
    }


def _valid_metadata_key(key: str) -> bool:
    return bool(key) and all(character.isascii() and (character.isalnum() or character in "-_.") for character in key)


def _valid_ascii_metadata_value(value: str) -> bool:
    return all(0x20 <= ord(character) <= 0x7E for character in value)


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


async def _send_json(
    send,
    status: int,
    payload: dict[str, Any],
    *,
    extra_headers: SequenceHeader = (),
    max_bytes: int = 0,
) -> None:
    data = json.dumps(payload).encode()
    if max_bytes > 0 and len(data) > max_bytes:
        raise InvariantError(
            grpc.StatusCode.RESOURCE_EXHAUSTED,
            f"encoded response exceeds {max_bytes} byte limit",
        )
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [
                (b"content-type", b"application/json"),
                (b"content-length", str(len(data)).encode()),
                *extra_headers,
            ],
        }
    )
    await send({"type": "http.response.body", "body": data})


async def _send_status(send, status: int, *, extra_headers: SequenceHeader = ()) -> None:
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [(b"content-length", b"0"), *extra_headers],
        }
    )
    await send({"type": "http.response.body", "body": b""})


async def _send_bytes(
    send,
    status: int,
    content_type: str,
    payload: bytes,
    *,
    extra_headers: SequenceHeader = (),
    max_bytes: int = 0,
) -> None:
    if max_bytes > 0 and len(payload) > max_bytes:
        raise InvariantError(
            grpc.StatusCode.RESOURCE_EXHAUSTED,
            f"encoded response exceeds {max_bytes} byte limit",
        )
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [
                (b"content-type", content_type.encode()),
                (b"content-length", str(len(payload)).encode()),
                *extra_headers,
            ],
        }
    )
    await send({"type": "http.response.body", "body": payload})


async def _send_error(
    send,
    e: Exception,
    *,
    extra_headers: SequenceHeader = (),
    max_bytes: int = 0,
) -> None:
    """Connect-style error envelope: {"code": "invalid_argument", "message": "...", ...}"""
    err = as_invariant_error(e)
    data = json.dumps(err.to_connect_payload()).encode()
    if max_bytes > 0 and len(data) > max_bytes:
        err = InvariantError(
            grpc.StatusCode.RESOURCE_EXHAUSTED,
            "encoded error response exceeds configured byte limit",
        )
        data = json.dumps(err.to_connect_payload()).encode()
        if len(data) > max_bytes:
            data = b""
    await send(
        {
            "type": "http.response.start",
            "status": http_status_for(err.code),
            "headers": [
                (b"content-type", b"application/json"),
                (b"content-length", str(len(data)).encode()),
                *extra_headers,
            ],
        }
    )
    await send({"type": "http.response.body", "body": data})


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
    if reserved := flags & ~0x03:
        raise InvariantError(
            grpc.StatusCode.INVALID_ARGUMENT,
            f"request envelope has unsupported reserved flags 0x{reserved:02x}",
        )
    if flags & 0x01:
        raise InvariantError(
            grpc.StatusCode.UNIMPLEMENTED,
            "compressed request envelopes are not supported",
        )
    if flags & CONNECT_END_STREAM_FLAG:
        raise InvariantError(
            grpc.StatusCode.INVALID_ARGUMENT,
            "request envelope must not use the end-stream flag",
        )
    size = (data[1] << 24) | (data[2] << 16) | (data[3] << 8) | data[4]
    if size > max_size:
        raise InvariantError(
            grpc.StatusCode.RESOURCE_EXHAUSTED,
            f"request envelope size {size} exceeds {max_size} byte limit",
        )
    if len(data) < 5 + size:
        raise InvariantError(
            grpc.StatusCode.INVALID_ARGUMENT,
            "stream request body truncated",
        )
    if len(data) != 5 + size:
        raise InvariantError(
            grpc.StatusCode.INVALID_ARGUMENT,
            "stream request body must contain exactly one envelope",
        )
    return flags, data[5 : 5 + size]


def _connect_control_payload(payload: dict[str, Any]) -> bytes:
    encoded = json.dumps(payload).encode()
    if len(encoded) <= CONNECT_CONTROL_MAX:
        return encoded
    fallback = InvariantError(
        grpc.StatusCode.RESOURCE_EXHAUSTED,
        "Connect control envelope exceeds configured byte limit",
    )
    return json.dumps({"error": fallback.to_connect_payload()}).encode()


async def _send_connect_stream_error(send, content_type: str, error: Exception) -> None:
    payload = _connect_control_payload({"error": as_invariant_error(error).to_connect_payload()})
    await send(
        {
            "type": "http.response.start",
            "status": 200,
            "headers": [(b"content-type", content_type.encode())],
        }
    )
    await send(
        {
            "type": "http.response.body",
            "body": _pack_envelope(CONNECT_END_STREAM_FLAG, payload),
            "more_body": False,
        }
    )


async def _serve_stream(
    send,
    receive,
    server: Server,
    route: _HTTPRoute,
    headers: dict[str, str],
    scope: dict[str, Any],
) -> None:
    """Serve a server-streaming RPC over Connect's streaming wire format.

    Request body: a single envelope wrapping the request (JSON or binary proto).
    Response body: zero or more message envelopes, then one end-stream envelope
    (flags=0x02) carrying either ``{}`` (success) or ``{"error": {...}}`` (failure).
    The end-stream payload is always JSON regardless of the message content type.
    """
    tool = route.tool
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
        body = await _read_body(receive, max_bytes=route.max_stream_request + 5)
    except InvariantError as e:
        await _send_connect_stream_error(send, CONNECT_STREAM_PROTO if binary else CONNECT_STREAM_JSON, e)
        return

    request = tool.new_request()
    try:
        _, payload = _unpack_envelope(body, max_size=route.max_stream_request)
        if binary:
            if payload:
                request.ParseFromString(payload)
        elif payload.strip():
            json_format.Parse(payload, request)
    except InvariantError as e:
        await _send_connect_stream_error(send, CONNECT_STREAM_PROTO if binary else CONNECT_STREAM_JSON, e)
        return
    except Exception as e:
        await _send_connect_stream_error(
            send,
            CONNECT_STREAM_PROTO if binary else CONNECT_STREAM_JSON,
            invalid_argument_from_json_error(e),
        )
        return

    timeout = _connect_timeout_seconds(headers)
    context = _http_projection_context(server, scope, headers, timeout)
    resp_ct = CONNECT_STREAM_PROTO if binary else CONNECT_STREAM_JSON
    started = False

    async def start_response() -> None:
        nonlocal started
        if started:
            return
        started = True
        context.mark_initial_sent()
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [
                    (b"content-type", resp_ct.encode()),
                    *_encode_response_metadata(context.initial_metadata()),
                ],
            }
        )

    context.set_initial_sender(start_response)
    timed_out = False
    try:
        stream_error: InvariantError | None = None
        try:
            if timeout is not None:
                async with asyncio.timeout(timeout):
                    await _drain_stream(
                        server,
                        tool,
                        request,
                        binary,
                        send,
                        context,
                        start_response,
                        route.max_stream_response,
                    )
            else:
                await _drain_stream(
                    server,
                    tool,
                    request,
                    binary,
                    send,
                    context,
                    start_response,
                    route.max_stream_response,
                )
        except TimeoutError as error:
            if timeout is None:
                stream_error = as_invariant_error(error)
            else:
                timed_out = True
                stream_error = InvariantError(
                    grpc.StatusCode.DEADLINE_EXCEEDED,
                    f"deadline exceeded after {timeout * 1000:.0f}ms",
                )
        except Exception as e:
            stream_error = as_invariant_error(e)

        await start_response()
        end: dict[str, Any] = {}
        if stream_error is not None:
            end["error"] = stream_error.to_connect_payload()
        if trailer := _connect_end_metadata(context.trailing_metadata()):
            end["metadata"] = trailer
        control_payload = _connect_control_payload(end)
        await send(
            {
                "type": "http.response.body",
                "body": _pack_envelope(CONNECT_END_STREAM_FLAG, control_payload),
                "more_body": False,
            }
        )
    finally:
        context.finish(cancelled=timed_out or context.cancelled())


async def _drain_stream(
    server: Server,
    tool,
    request,
    binary: bool,
    send,
    context: ProjectionContext,
    start_response,
    max_response_bytes: int,
) -> None:
    """Iterate the stream handler and pack each message into an envelope."""
    async for msg in server._invoke_stream(tool, request, context):
        if binary:
            payload = msg.SerializeToString()
        else:
            payload = json_format.MessageToJson(msg, preserving_proto_field_name=True, indent=None).encode()
        if max_response_bytes > 0 and len(payload) > max_response_bytes:
            raise InvariantError(
                grpc.StatusCode.RESOURCE_EXHAUSTED,
                f"encoded stream response message exceeds {max_response_bytes} byte limit",
            )
        await start_response()
        await send(
            {
                "type": "http.response.body",
                "body": _pack_envelope(0, payload),
                "more_body": True,
            }
        )


async def _serve_mcp_http(
    send,
    receive,
    server: Server,
    headers: dict[str, str],
    scope: dict[str, Any],
) -> None:
    """Serve one MCP JSON-RPC request over POST /mcp.

    Reads one JSON-RPC envelope, dispatches through the shared mcp_dispatch,
    and returns the JSON-RPC response. Accepted notifications and client
    responses get a 202 with an empty body.
    """
    # Local import avoids a module-load cycle with the shared dispatcher.
    from invariant.projections.mcp import _PROTOCOL_VERSION, mcp_dispatch

    if "origin" in headers:
        await _send_status(send, 403)
        return
    if not _accepts_mcp_response_types(headers.get("accept", "")):
        await _send_status(send, 406)
        return

    try:
        body = await _read_body(receive, max_bytes=server._http_max_unary_request)
    except InvariantError as e:
        await _send_error(send, e, max_bytes=server._http_max_unary_response)
        return

    try:
        msg = json.loads(body)
    except json.JSONDecodeError as e:
        try:
            await _send_json(
                send,
                200,
                {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": f"Parse error: {e}"}},
                max_bytes=server._http_max_unary_response,
            )
        except InvariantError as limit_error:
            await _send_error(send, limit_error, max_bytes=server._http_max_unary_response)
        return

    if not isinstance(msg, dict):
        try:
            await _send_json(
                send,
                200,
                {"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "Invalid Request"}},
                max_bytes=server._http_max_unary_response,
            )
        except InvariantError as limit_error:
            await _send_error(send, limit_error, max_bytes=server._http_max_unary_response)
        return

    protocol_version = headers.get("mcp-protocol-version", "")
    method = msg.get("method")
    if (method == "initialize" and protocol_version not in ("", _PROTOCOL_VERSION)) or (
        method != "initialize" and protocol_version != _PROTOCOL_VERSION
    ):
        await _send_status(send, 400)
        return

    if "method" not in msg and "id" in msg and ("result" in msg or "error" in msg):
        await _send_status(send, 202)
        return
    if msg.get("id") is None:
        await _send_status(send, 202)
        return

    timeout = _connect_timeout_seconds(headers)
    context = _http_projection_context(server, scope, headers, timeout)
    try:
        if timeout is not None:
            async with asyncio.timeout(timeout):
                response = await mcp_dispatch(server, msg, context=context)
        else:
            response = await mcp_dispatch(server, msg, context=context)
    except TimeoutError as error:
        context.finish(cancelled=True)
        if timeout is None:
            err = as_invariant_error(error)
        else:
            err = InvariantError(
                grpc.StatusCode.DEADLINE_EXCEEDED,
                f"deadline exceeded after {timeout * 1000:.0f}ms",
            )
        await _send_error(
            send,
            err,
            extra_headers=_context_response_headers(context),
            max_bytes=server._http_max_unary_response,
        )
        return
    except Exception as e:
        await _send_error(
            send,
            e,
            extra_headers=_context_response_headers(context),
            max_bytes=server._http_max_unary_response,
        )
        return
    finally:
        context.finish(cancelled=context.cancelled())

    if response is None:
        await _send_status(send, 202, extra_headers=_context_response_headers(context))
        return
    try:
        await _send_json(
            send,
            200,
            response,
            extra_headers=_context_response_headers(context),
            max_bytes=server._http_max_unary_response,
        )
    except InvariantError as error:
        await _send_error(
            send,
            error,
            extra_headers=_context_response_headers(context),
            max_bytes=server._http_max_unary_response,
        )


def _accepts_mcp_response_types(value: str) -> bool:
    accepted: set[str] = set()
    for raw_part in value.split(","):
        parts = [part.strip() for part in raw_part.split(";")]
        media_type = parts[0].lower()
        if not media_type:
            continue
        parameters = {
            key.strip().lower(): val.strip() for part in parts[1:] if "=" in part for key, val in [part.split("=", 1)]
        }
        if "q" in parameters:
            try:
                if float(parameters["q"]) <= 0:
                    continue
            except ValueError:
                continue
        accepted.add(media_type)
    return {"application/json", "text/event-stream"}.issubset(accepted)
