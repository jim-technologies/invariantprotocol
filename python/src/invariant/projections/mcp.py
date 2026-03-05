"""MCP (Model Context Protocol) projection — stdio transport, JSON-RPC 2.0."""

from __future__ import annotations

import json
import sys
from typing import TYPE_CHECKING

from google.protobuf import descriptor_pool, json_format, message_factory

from invariant.errors import as_invariant_error, invalid_argument_from_json_error

if TYPE_CHECKING:
    from invariant.server import Server

_PROTOCOL_VERSION = "2024-11-05"


def serve_mcp(server: Server) -> None:
    """Run MCP server over stdio (newline-delimited JSON-RPC 2.0)."""
    _StdioMCP(server).run()


class _StdioMCP:
    def __init__(self, server: Server):
        self.server = server
        self._pool = descriptor_pool.Default()

    def run(self) -> None:
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue

            resp = self._dispatch(msg)
            if resp is not None:
                sys.stdout.write(json.dumps(resp) + "\n")
                sys.stdout.flush()

    def _dispatch(self, msg: dict) -> dict | None:
        method = msg.get("method", "")
        msg_id = msg.get("id")
        params = msg.get("params", {})

        # Notifications have no id — no response
        if msg_id is None:
            return None

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
            tools = sorted(self.server.tools.values(), key=lambda t: t.name)
            return _ok(
                msg_id,
                {
                    "tools": [
                        {
                            "name": t.name,
                            "description": t.description,
                            "inputSchema": t.input_schema,
                        }
                        for t in tools
                    ],
                },
            )

        if method == "tools/call":
            return self._call_tool(msg_id, params)

        if method == "ping":
            return _ok(msg_id, {})

        return _err(msg_id, -32601, f"Method not found: {method}")

    def _call_tool(self, msg_id: int, params: dict) -> dict:
        tool_name = params.get("name", "")
        arguments = params.get("arguments", {})

        tool = self.server.tools.get(tool_name)
        if tool is None:
            return _err(msg_id, -32602, f"Unknown tool: {tool_name}")

        try:
            request = self._build_request(tool.input_type, arguments)
            response = self.server._invoke(tool, request, None)

            if response is not None:
                result_dict = json_format.MessageToDict(response, preserving_proto_field_name=True)
                text = json.dumps(result_dict, indent=2)
            else:
                text = "{}"

            return _ok(
                msg_id,
                {
                    "content": [{"type": "text", "text": text}],
                },
            )
        except Exception as e:
            err = as_invariant_error(e)
            return _ok(
                msg_id,
                {
                    "content": [{"type": "text", "text": err.message}],
                    "isError": True,
                    "error": err.to_payload(),
                },
            )

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


def _ok(msg_id: int, result: dict) -> dict:
    return {"jsonrpc": "2.0", "id": msg_id, "result": result}


def _err(msg_id: int, code: int, message: str) -> dict:
    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": code, "message": message}}
