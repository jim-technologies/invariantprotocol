"""MCP (Model Context Protocol) projection — async stdio, JSON-RPC 2.0.

Supports cancellation via `notifications/cancelled`: an in-flight tool call
whose request id matches the notification's `requestId` is cancelled.
"""

from __future__ import annotations

import asyncio
import json
import sys
from typing import TYPE_CHECKING

from google.protobuf import descriptor_pool, json_format, message_factory

from invariant.errors import (
    InvariantError,
    as_invariant_error,
    invalid_argument_from_json_error,
)

if TYPE_CHECKING:
    from invariant.server import Server

_PROTOCOL_VERSION = "2024-11-05"


async def serve_mcp(server: Server) -> None:
    """Run MCP server over stdio (newline-delimited JSON-RPC 2.0). Blocks until stdin closes."""
    await _StdioMCP(server).run()


class _StdioMCP:
    def __init__(self, server: Server):
        self.server = server
        self._pool = descriptor_pool.Default()
        # in-flight tools/call tasks keyed by JSON-RPC request id; cancelled on
        # notifications/cancelled. Registered synchronously in the read loop
        # before the task starts, so a notification arriving on the next read
        # always finds the entry.
        self._inflight: dict[str, asyncio.Task] = {}

    async def run(self) -> None:
        reader = await _stdin_reader()

        background: list[asyncio.Task] = []
        try:
            while True:
                line = await reader.readline()
                if not line:
                    break
                line = line.strip()
                if not line:
                    continue

                try:
                    msg = json.loads(line)
                except json.JSONDecodeError as e:
                    _write_response(_err(None, -32700, f"Parse error: {e}"))
                    continue

                # Notifications run synchronously — cancellation must take effect
                # before the next request is read.
                if msg.get("id") is None:
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
                task = asyncio.create_task(self._handle_request(msg))
                # Register synchronously *before* yielding to the event loop so
                # an immediately-following notifications/cancelled finds it.
                self._inflight[key] = task
                task.add_done_callback(lambda _t, k=key: self._inflight.pop(k, None))
                background.append(task)
        finally:
            # On stdin EOF, wait for in-flight tools/call to finish so callers
            # see their responses. Tasks are only cancelled if the run() coroutine
            # itself is cancelled (passed through the gather below).
            if background:
                await asyncio.gather(*background, return_exceptions=True)

    async def _handle_request(self, msg: dict) -> None:
        resp = await self._dispatch(msg)
        if resp is not None:
            _write_response(resp)

    def _handle_notification(self, msg: dict) -> None:
        if msg.get("method") != "notifications/cancelled":
            return
        params = msg.get("params") or {}
        request_id = params.get("requestId")
        if request_id is None:
            return
        task = self._inflight.get(_id_key(request_id))
        if task is not None:
            task.cancel()

    async def _dispatch(self, msg: dict) -> dict | None:
        return await mcp_dispatch(self.server, msg)

    async def _call_tool(self, msg_id, params: dict) -> dict:
        return await mcp_call_tool(self.server, msg_id, params)


_dispatch_pool = descriptor_pool.Default()


def _build_request(type_name: str, arguments: dict):
    """Construct a typed proto request from JSON args, raising InvariantError on schema mismatch."""
    try:
        desc = _dispatch_pool.FindMessageTypeByName(type_name)
    except KeyError as e:
        raise ValueError(
            f"Message type '{type_name}' not found in descriptor pool. "
            f"Make sure the corresponding _pb2 module is imported."
        ) from e
    msg_class = message_factory.GetMessageClass(desc)
    msg = msg_class()
    try:
        json_format.ParseDict(arguments, msg)
    except Exception as e:
        raise invalid_argument_from_json_error(e) from None
    return msg


async def mcp_dispatch(server: Server, msg: dict) -> dict | None:
    """Dispatch a single MCP JSON-RPC request.

    Shared by stdio and the HTTP /mcp transport. Returns None for
    notifications (no id field); otherwise returns the JSON-RPC response dict.
    """
    method = msg.get("method", "")
    msg_id = msg.get("id")
    params = msg.get("params", {})

    if msg_id is None:
        # Notification — caller decides what to do (stdio: nothing; HTTP: 204).
        return None

    if method == "initialize":
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
        return await mcp_call_tool(server, msg_id, params)

    if method == "ping":
        return _ok(msg_id, {})

    return _err(msg_id, -32601, f"Method not found: {method}")


async def mcp_call_tool(server: Server, msg_id, params: dict) -> dict:
    """Execute a tools/call — unary or server-streaming."""
    tool_name = params.get("name", "")
    arguments = params.get("arguments", {})

    tool = server.tools.get(tool_name)
    if tool is None:
        return _err(msg_id, -32602, f"Unknown tool: {tool_name}")

    if tool.server_streaming:
        return await _call_stream_tool(server, msg_id, tool, arguments)

    try:
        request = _build_request(tool.input_type, arguments)
        # Don't swallow CancelledError — let it propagate. Stdio's task scheduler
        # cleans up cancelled requests without a response (per MCP spec); HTTP's
        # asyncio.timeout converts it to deadline_exceeded.
        response = await server._invoke(tool, request, None)

        if response is not None:
            result_dict = json_format.MessageToDict(response, preserving_proto_field_name=True)
            text = json.dumps(result_dict, indent=2)
        else:
            text = "{}"

        return _ok(msg_id, {"content": [{"type": "text", "text": text}]})
    except Exception as e:
        return _error_result(msg_id, as_invariant_error(e))


async def _call_stream_tool(server: Server, msg_id, tool, arguments: dict) -> dict:
    """Run a server-streaming tool, collecting each chunk as a text block in
    the response content. Errors mid-stream become an isError result preserving
    whatever chunks were emitted first. CancelledError propagates so callers
    (stdio task scheduler, HTTP asyncio.timeout) can handle it correctly.
    """
    try:
        request = _build_request(tool.input_type, arguments)
    except Exception as e:
        return _error_result(msg_id, as_invariant_error(e))

    content: list[dict] = []
    try:
        async for response in server._invoke_stream(tool, request, None):
            chunk = json_format.MessageToDict(response, preserving_proto_field_name=True)
            content.append({"type": "text", "text": json.dumps(chunk, indent=2)})
    except Exception as e:
        err = as_invariant_error(e)
        content.append({"type": "text", "text": err.message})
        return _ok(msg_id, {"content": content, "isError": True, "error": err.to_payload()})

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
