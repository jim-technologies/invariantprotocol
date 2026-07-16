"""Test MCP projection."""

import asyncio
import json
import os
import subprocess
import sys

from conftest import DESCRIPTOR_PATH, register_greet

from invariant import Server


def _mcp_request(msg_id, method, params=None):
    msg = {"jsonrpc": "2.0", "id": msg_id, "method": method}
    if params is not None:
        msg["params"] = params
    return json.dumps(msg)


def _run_mcp_session(messages: list[str]) -> list[dict]:
    """Send JSON-RPC messages to the MCP server via stdio and collect responses."""
    stdin_data = "\n".join(messages) + "\n"

    test_dir = os.path.dirname(os.path.abspath(__file__))
    src_dir = os.path.join(test_dir, "..", "src")
    gen_dir = os.path.join(test_dir, "proto", "gen")
    descriptor = os.path.join(test_dir, "proto", "descriptor.binpb")

    script = f"""
import asyncio
import sys
sys.path.insert(0, {src_dir!r})
sys.path.insert(0, {gen_dir!r})
import greet_pb2
import greet_pb2_grpc
from invariant import Server

class GreetServicer:
    async def Greet(self, request, context):
        return greet_pb2.GreetResponse(
            message=f"Hi {{request.name}}",
            mood=request.mood,
            tags=dict(request.tags),
        )
    async def GreetGroup(self, request, context):
        messages = [f"Hi {{p.name}}" for p in request.people]
        return greet_pb2.GreetGroupResponse(messages=messages, count=len(request.people))
    async def StreamGreet(self, request, context):
        if False:
            yield greet_pb2.GreetResponse()

async def main():
    server = Server.from_descriptor({descriptor!r})
    server.exclude("*StreamGreet")
    greet_pb2_grpc.add_GreetServiceServicer_to_server(GreetServicer(), server)
    await server.serve_projections(mcp=True)

asyncio.run(main())
"""
    proc = subprocess.run(
        [sys.executable, "-c", script],
        input=stdin_data,
        capture_output=True,
        text=True,
        timeout=10,
    )

    responses = []
    for line in proc.stdout.strip().split("\n"):
        if line.strip():
            responses.append(json.loads(line))
    return responses


def test_mcp_initialize():
    responses = _run_mcp_session(
        [
            _mcp_request(
                0,
                "initialize",
                {
                    "protocolVersion": "2025-11-25",
                    "capabilities": {},
                    "clientInfo": {"name": "test", "version": "1.0"},
                },
            ),
        ]
    )
    assert len(responses) == 1
    assert responses[0]["result"]["protocolVersion"] == "2025-11-25"
    assert responses[0]["result"]["serverInfo"]["name"] == "invariant-protocol"


def test_mcp_tools_list():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(1, "tools/list", {}),
        ]
    )
    tools = responses[1]["result"]["tools"]
    assert len(tools) == 2
    assert [t["name"] for t in tools] == ["GreetService.Greet", "GreetService.GreetGroup"]
    tools_by_name = {t["name"]: t for t in tools}
    assert "GreetService.Greet" in tools_by_name
    assert tools_by_name["GreetService.Greet"]["description"] == "Greet a person by name."
    assert "name" in tools_by_name["GreetService.Greet"]["inputSchema"]["properties"]
    assert "GreetService.GreetGroup" in tools_by_name
    assert tools_by_name["GreetService.GreetGroup"]["description"] == "Greet multiple people at once."


def test_mcp_tool_call():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "GreetService.Greet",
                    "arguments": {"name": "World"},
                },
            ),
        ]
    )
    content = responses[1]["result"]["content"]
    assert len(content) == 1
    result = json.loads(content[0]["text"])
    assert result["message"] == "Hi World"


def test_mcp_tool_call_rejects_unknown_field():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "GreetService.Greet",
                    "arguments": {"name": "World", "extra": "x"},
                },
            ),
        ]
    )
    result = responses[1]["result"]
    assert result["isError"] is True
    assert result["error"]["code"] == "invalid_argument"
    assert 'field named "extra"' in result["error"]["message"]
    assert result["error"]["details"][0]["fieldViolations"][0]["field"] == "extra"


def test_mcp_tool_call_with_enum_and_tags():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "GreetService.Greet",
                    "arguments": {"name": "World", "mood": "MOOD_HAPPY", "tags": {"lang": "en"}},
                },
            ),
        ]
    )
    result = json.loads(responses[1]["result"]["content"][0]["text"])
    assert result["message"] == "Hi World"
    assert result["mood"] == "MOOD_HAPPY"
    assert result["tags"]["lang"] == "en"


def test_mcp_tool_call_greet_group():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "GreetService.GreetGroup",
                    "arguments": {
                        "people": [
                            {"name": "Alice", "mood": "MOOD_HAPPY"},
                            {"name": "Bob"},
                        ],
                    },
                },
            ),
        ]
    )
    result = json.loads(responses[1]["result"]["content"][0]["text"])
    assert result["messages"] == ["Hi Alice", "Hi Bob"]
    assert result["count"] == 2


def test_mcp_tool_call_unknown():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "does_not_exist",
                    "arguments": {},
                },
            ),
        ]
    )
    assert "error" in responses[1] or responses[1].get("result", {}).get("isError") is not None


def test_mcp_ping():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(1, "ping", {}),
        ]
    )
    assert responses[1]["result"] == {}


def test_mcp_unknown_method():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", {}),
            _mcp_request(1, "unknown/method", {}),
        ]
    )
    assert "error" in responses[1]
    assert responses[1]["error"]["code"] == -32601


async def test_mcp_stdio_cancel_notification_cancels_inflight_tool(monkeypatch):
    from invariant.projections import mcp

    started = asyncio.Event()
    cancelled = asyncio.Event()
    context_was_cancelled: list[bool] = []

    class BlockingServicer:
        async def Greet(self, request, context):
            started.set()
            try:
                await asyncio.Event().wait()
            finally:
                context_was_cancelled.append(context.cancelled())
                cancelled.set()

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(server, BlockingServicer())
    reader = asyncio.StreamReader()
    responses: list[dict] = []

    async def stdin_reader():
        return reader

    monkeypatch.setattr(mcp, "_stdin_reader", stdin_reader)
    monkeypatch.setattr(mcp, "_write_response", responses.append)

    runner = asyncio.create_task(mcp.serve_mcp(server))
    try:
        call = {
            "jsonrpc": "2.0",
            "id": "call-1",
            "method": "tools/call",
            "params": {"name": "GreetService.Greet", "arguments": {"name": "blocked"}},
        }
        reader.feed_data((json.dumps(call) + "\n").encode())
        await asyncio.wait_for(started.wait(), timeout=2)

        cancellation = {
            "jsonrpc": "2.0",
            "method": "notifications/cancelled",
            "params": {"requestId": "call-1", "reason": "client no longer needs it"},
        }
        reader.feed_data((json.dumps(cancellation) + "\n").encode())
        reader.feed_eof()

        await asyncio.wait_for(runner, timeout=2)
        await asyncio.wait_for(cancelled.wait(), timeout=2)
        assert context_was_cancelled == [True]
        assert responses == []
    finally:
        if not runner.done():
            runner.cancel()
            await asyncio.gather(runner, return_exceptions=True)
        await server.stop(grace=0)
