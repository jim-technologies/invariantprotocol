"""MCP (Model Context Protocol) projection — async stdio, JSON-RPC 2.0.

Supports cancellation via `notifications/cancelled`: an in-flight tool call
whose request id matches the notification's `requestId` is cancelled.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import sys
from collections.abc import AsyncGenerator
from typing import TYPE_CHECKING, Any, cast

import grpc
from google.protobuf import json_format

from invariant.errors import (
    InvariantError,
    as_invariant_error,
    invalid_argument_from_json_error,
)
from invariant.projection_context import ProjectionContext

if TYPE_CHECKING:
    from invariant.server import Server

_PROTOCOL_VERSION = "2025-11-25"
_MAX_SAFE_INTEGER_ID = (1 << 53) - 1
_RESPONSE_LIMIT_MESSAGE = "encoded MCP response exceeds configured byte limit"


async def serve_mcp(server: Server) -> None:
    """Run MCP server over stdio (newline-delimited JSON-RPC 2.0). Blocks until stdin closes."""
    await _StdioMCP(server).run()


class _StdioMCP:
    def __init__(self, server: Server):
        self.server = server
        # in-flight tools/call tasks keyed by JSON-RPC request id; cancelled on
        # notifications/cancelled. Registered synchronously in the read loop
        # before the task starts, so a notification arriving on the next read
        # always finds the entry.
        self._inflight: dict[str, asyncio.Task] = {}
        self._protocol_cancelled: set[str] = set()
        self._background: set[asyncio.Task[None]] = set()

    async def run(self) -> None:
        reader = await _stdin_reader()

        try:
            while True:
                line = await reader.readline()
                if not line:
                    break
                line = line.strip()
                if not line:
                    continue

                try:
                    msg = _parse_json(line)
                except ValueError as e:
                    _write_response(_err(None, -32700, f"Parse error: {e}"))
                    continue
                if invalid := _invalid_request_response(msg):
                    _write_response(invalid)
                    continue
                if _is_client_response(msg):
                    continue
                if not isinstance(msg, dict):
                    continue

                # Notifications run synchronously — cancellation must take effect
                # before the next request is read.
                if "id" not in msg:
                    self._handle_notification(msg)
                    continue

                # tools/call is the only method that can block on user code —
                # dispatch it concurrently so notifications/cancelled can
                # interrupt it. Fast metadata methods (initialize, tools/list,
                # ping) run inline to keep response order deterministic.
                if msg.get("method") != "tools/call":
                    resp = await self._dispatch(msg)
                    if resp is not None:
                        _write_response(resp)
                    continue

                msg_id = msg.get("id")
                key = _id_key(msg_id)
                task = asyncio.create_task(self._handle_request(msg, key))
                # Register synchronously *before* yielding to the event loop so
                # an immediately-following notifications/cancelled finds it.
                self._inflight[key] = task

                def finished(_task, *, request_key=key):
                    self._inflight.pop(request_key, None)
                    self._protocol_cancelled.discard(request_key)
                    self._background.discard(_task)

                task.add_done_callback(finished)
                self._background.add(task)
        except BaseException:
            # Cancelling the stdio projection must also cancel application work;
            # otherwise a multi-projection shutdown could hang on a tool call.
            for task in self._background:
                if not task.done():
                    task.cancel()
            if self._background:
                await asyncio.gather(*self._background, return_exceptions=True)
            raise
        else:
            # Normal stdin EOF drains calls that were not explicitly cancelled,
            # so their responses are not lost merely because input closed.
            if self._background:
                await asyncio.gather(*self._background, return_exceptions=True)

    async def _handle_request(self, msg: dict, key: str) -> None:
        resp = await self._dispatch(msg)
        if key not in self._protocol_cancelled and resp is not None:
            _write_response(resp)

    def _handle_notification(self, msg: dict) -> None:
        if msg.get("method") != "notifications/cancelled":
            return
        params = msg.get("params")
        if not isinstance(params, dict):
            return
        request_id = params.get("requestId")
        if not _valid_jsonrpc_id(request_id):
            return
        key = _id_key(request_id)
        task = self._inflight.get(key)
        if task is not None:
            self._protocol_cancelled.add(key)
            task.cancel()

    async def _dispatch(self, msg: dict) -> dict | None:
        return await mcp_dispatch(self.server, msg)

    async def _call_tool(self, msg_id, params: dict) -> dict:
        return await mcp_call_tool(self.server, msg_id, params)


def _build_request(tool, arguments: dict):
    """Construct a typed proto request from JSON args, raising InvariantError on schema mismatch."""
    msg = tool.new_request()
    try:
        json_format.ParseDict(arguments, msg)
    except Exception as e:
        raise invalid_argument_from_json_error(e) from None
    return msg


async def mcp_dispatch(
    server: Server,
    msg: dict,
    context: ProjectionContext | None = None,
    *,
    max_response_bytes: int | None = None,
) -> dict | None:
    """Dispatch a single MCP JSON-RPC request.

    Shared by stdio and the HTTP /mcp transport. Returns None for
    notifications (no id field); otherwise returns the JSON-RPC response dict.
    """
    server._freeze()
    if invalid := _invalid_request_response(msg):
        return invalid
    if _is_client_response(msg):
        return None

    method = msg.get("method", "")
    msg_id = msg.get("id")
    params = msg.get("params", {})

    if "id" not in msg:
        # Notification — caller decides what to do (stdio: nothing; HTTP: 202).
        return None
    if not isinstance(params, dict):
        return _err(msg_id, -32602, "Invalid params")

    if method == "initialize":
        client_info = params.get("clientInfo")
        if (
            not isinstance(params.get("protocolVersion"), str)
            or not isinstance(params.get("capabilities"), dict)
            or not isinstance(client_info, dict)
            or not isinstance(client_info.get("name"), str)
            or not isinstance(client_info.get("version"), str)
        ):
            return _err(msg_id, -32602, "Invalid params")
        return _ok(
            msg_id,
            {
                "protocolVersion": _PROTOCOL_VERSION,
                "capabilities": {"tools": {}},
                "serverInfo": {"name": server.name, "version": server.version},
            },
        )

    if method == "tools/list":
        return _ok(msg_id, {"tools": server.tool_catalog()})

    if method == "tools/call":
        if not isinstance(params.get("name"), str) or not isinstance(params.get("arguments", {}), dict):
            return _err(msg_id, -32602, "Invalid params")
        return await mcp_call_tool(
            server,
            msg_id,
            params,
            context=context,
            max_response_bytes=max_response_bytes,
        )

    if method == "ping":
        return _ok(msg_id, {})

    return _err(msg_id, -32601, f"Method not found: {method}")


def _invalid_request_response(msg: object) -> dict | None:
    if not isinstance(msg, dict):
        return _err(None, -32600, "Invalid Request")
    if _is_client_response(msg):
        return None
    request_id = msg.get("id")
    if (
        msg.get("jsonrpc") != "2.0"
        or not isinstance(msg.get("method"), str)
        or ("id" in msg and not _valid_jsonrpc_id(request_id))
    ):
        return _err(None, -32600, "Invalid Request")
    return None


def _is_client_response(msg: object) -> bool:
    if not isinstance(msg, dict):
        return False
    if msg.get("jsonrpc") != "2.0" or "method" in msg:
        return False
    if ("result" in msg) == ("error" in msg):
        return False
    if "result" in msg:
        return "id" in msg and _valid_jsonrpc_id(msg.get("id")) and isinstance(msg.get("result"), dict)

    if "id" in msg and not _valid_jsonrpc_id(msg.get("id")):
        return False
    error = msg.get("error")
    if not isinstance(error, dict):
        return False
    code = error.get("code")
    return isinstance(code, int) and not isinstance(code, bool) and isinstance(error.get("message"), str)


def _valid_jsonrpc_id(value: object) -> bool:
    return isinstance(value, str) or (
        isinstance(value, int)
        and not isinstance(value, bool)
        and -_MAX_SAFE_INTEGER_ID <= value <= _MAX_SAFE_INTEGER_ID
    )


def _parse_json(value: str | bytes) -> object:
    def reject_constant(constant: str):
        raise ValueError(f"non-finite number {constant!r} is not valid JSON")

    return json.loads(value, parse_constant=reject_constant)


async def mcp_call_tool(
    server: Server,
    msg_id,
    params: dict,
    *,
    context: ProjectionContext | None = None,
    max_response_bytes: int | None = None,
) -> dict:
    """Execute a tools/call — unary or server-streaming."""
    tool_name = params.get("name", "")
    arguments = params.get("arguments", {})

    tool = server._tools.get(tool_name)
    if tool is None:
        return _err(msg_id, -32602, f"Unknown tool: {tool_name}")

    owns_context = context is None
    if context is None:
        context = ProjectionContext(peer="invariant:mcp")

    if tool.server_streaming:
        try:
            return await _call_stream_tool(
                server,
                msg_id,
                tool,
                arguments,
                context,
                max_response_bytes=max_response_bytes,
            )
        finally:
            if owns_context:
                context.finish(cancelled=context.cancelled())

    try:
        request = _build_request(tool, arguments)
        # Don't swallow CancelledError — let it propagate. Stdio's task scheduler
        # cleans up cancelled requests without a response (per MCP spec); HTTP's
        # asyncio.timeout converts it to deadline_exceeded.
        response = await server._invoke(tool, request, context)

        if response is not None:
            result_dict = json_format.MessageToDict(response)
            text = json.dumps(result_dict, indent=2)
        else:
            text = "{}"

        return _ok(msg_id, {"content": [{"type": "text", "text": text}]})
    except Exception as e:
        return _error_result(msg_id, as_invariant_error(e))
    finally:
        if owns_context:
            context.finish(cancelled=context.cancelled())


async def _call_stream_tool(
    server: Server,
    msg_id,
    tool,
    arguments: dict,
    context: ProjectionContext,
    *,
    max_response_bytes: int | None,
) -> dict:
    """Run a server-streaming tool, collecting each chunk as a text block in
    the response content. Errors mid-stream become an isError result preserving
    whatever chunks were emitted first. CancelledError propagates so callers
    (stdio task scheduler, HTTP asyncio.timeout) can handle it correctly.
    """
    try:
        request = _build_request(tool, arguments)
    except Exception as e:
        return _error_result(msg_id, as_invariant_error(e))

    content: list[dict] = []
    encoded_response_bytes = len(json.dumps(_ok(msg_id, {"content": content})).encode())
    if max_response_bytes is not None and encoded_response_bytes > max_response_bytes:
        raise InvariantError(grpc.StatusCode.RESOURCE_EXHAUSTED, _RESPONSE_LIMIT_MESSAGE)

    responses = cast(AsyncGenerator[Any], server._invoke_stream(tool, request, context))
    response_limit_error: InvariantError | None = None
    try:
        async for response in responses:
            chunk = json_format.MessageToDict(response)
            block = {"type": "text", "text": json.dumps(chunk, indent=2)}
            next_response_bytes = encoded_response_bytes + len(json.dumps(block).encode())
            if content:
                next_response_bytes += len(", ")
            if max_response_bytes is not None and next_response_bytes > max_response_bytes:
                response_limit_error = InvariantError(
                    grpc.StatusCode.RESOURCE_EXHAUSTED,
                    _RESPONSE_LIMIT_MESSAGE,
                )
                raise response_limit_error
            content.append(block)
            encoded_response_bytes = next_response_bytes
    except Exception as e:
        if e is response_limit_error:
            raise
        err = as_invariant_error(e)
        content.append({"type": "text", "text": err.message})
        return _ok(msg_id, {"content": content, "isError": True, "error": err.to_payload()})
    finally:
        if response_limit_error is None:
            await responses.aclose()
        else:
            with contextlib.suppress(Exception):
                await responses.aclose()

    return _ok(msg_id, {"content": content})


async def _stdin_reader() -> asyncio.StreamReader:
    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    loop = asyncio.get_event_loop()
    await loop.connect_read_pipe(lambda: protocol, sys.stdin)
    return reader


def _write_response(resp: dict) -> None:
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()


def _id_key(msg_id) -> str:
    """JSON-RPC ids may be string or number; normalize for dict keys."""
    return f"{type(msg_id).__name__}:{msg_id}"


def _ok(msg_id, result: dict) -> dict:
    return {"jsonrpc": "2.0", "id": msg_id, "result": result}


def _err(msg_id, code: int, message: str) -> dict:
    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": code, "message": message}}


def _error_result(msg_id, err: InvariantError) -> dict:
    """tools/call result envelope for any InvariantError."""
    return _ok(
        msg_id,
        {
            "content": [{"type": "text", "text": err.message}],
            "isError": True,
            "error": err.to_payload(),
        },
    )
