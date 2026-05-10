"""MCP (Model Context Protocol) projection — async stdio, JSON-RPC 2.0.

Supports cancellation via `notifications/cancelled`: an in-flight tool call
whose request id matches the notification's `requestId` is cancelled.
"""

from __future__ import annotations

import asyncio
import json
import sys
from typing import TYPE_CHECKING

import grpc
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
        method = msg.get("method", "")
        msg_id = msg.get("id")
        params = msg.get("params", {})

        if method == "initialize":
            return _ok(
                msg_id,
                {
                    "protocolVersion": _PROTOCOL_VERSION,
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": self.server.name, "version": self.server.version},
                },
            )

        if method == "tools/list":
            return _ok(msg_id, {"tools": self.server.tool_catalog()})

        if method == "tools/call":
            return await self._call_tool(msg_id, params)

        if method == "ping":
            return _ok(msg_id, {})

        return _err(msg_id, -32601, f"Method not found: {method}")

    async def _call_tool(self, msg_id, params: dict) -> dict:
        tool_name = params.get("name", "")
        arguments = params.get("arguments", {})

        tool = self.server.tools.get(tool_name)
        if tool is None:
            return _err(msg_id, -32602, f"Unknown tool: {tool_name}")

        try:
            request = self._build_request(tool.input_type, arguments)
            try:
                response = await self.server._invoke(tool, request, None)
            except asyncio.CancelledError:
                return _error_result(msg_id, InvariantError(grpc.StatusCode.CANCELLED, "request cancelled"))

            if response is not None:
                result_dict = json_format.MessageToDict(response, preserving_proto_field_name=True)
                text = json.dumps(result_dict, indent=2)
            else:
                text = "{}"

            return _ok(msg_id, {"content": [{"type": "text", "text": text}]})
        except Exception as e:
            return _error_result(msg_id, as_invariant_error(e))

    def _build_request(self, type_name: str, arguments: dict):
        try:
            desc = self._pool.FindMessageTypeByName(type_name)
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
