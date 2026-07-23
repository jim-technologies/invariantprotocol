"""Test MCP projection."""

import asyncio
import json
import os
import subprocess
import sys

import greet_pb2
from conftest import DESCRIPTOR_PATH, register_greet

from invariant import Server


def _mcp_request(msg_id, method, params=None):
    msg = {"jsonrpc": "2.0", "id": msg_id, "method": method}
    if params is not None:
        msg["params"] = params
    return json.dumps(msg)


def _initialize_params(protocol_version: str = "2025-11-25") -> dict:
    return {
        "protocolVersion": protocol_version,
        "capabilities": {},
        "clientInfo": {"name": "invariant-test", "version": "1.0"},
    }


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


def test_mcp_initialize_validates_params_and_negotiates_version():
    invalid_params = [
        None,
        {},
        {"protocolVersion": 1, "capabilities": {}, "clientInfo": {"name": "test", "version": "1"}},
        {"protocolVersion": "2025-11-25", "capabilities": [], "clientInfo": {"name": "test", "version": "1"}},
        {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": []},
        {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": 1, "version": "1"}},
        {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "test", "version": 1}},
    ]
    responses = _run_mcp_session(
        [
            *[_mcp_request(index + 10, "initialize", params) for index, params in enumerate(invalid_params)],
            _mcp_request(20, "initialize", _initialize_params("2099-01-01")),
        ]
    )
    assert len(responses) == len(invalid_params) + 1
    for index, response in enumerate(responses[:-1]):
        assert response["id"] == index + 10
        assert response["error"]["code"] == -32602
    assert responses[-1]["result"]["protocolVersion"] == "2025-11-25"


def test_mcp_tools_list():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", _initialize_params()),
            _mcp_request(1, "tools/list", {}),
        ]
    )
    tools = responses[1]["result"]["tools"]
    assert len(tools) == 2
    assert [t["name"] for t in tools] == ["greet.v1.GreetService.Greet", "greet.v1.GreetService.GreetGroup"]
    tools_by_name = {t["name"]: t for t in tools}
    assert "greet.v1.GreetService.Greet" in tools_by_name
    assert tools_by_name["greet.v1.GreetService.Greet"]["description"] == "Greet a person by name."
    assert "name" in tools_by_name["greet.v1.GreetService.Greet"]["inputSchema"]["properties"]
    assert "greet.v1.GreetService.GreetGroup" in tools_by_name
    assert tools_by_name["greet.v1.GreetService.GreetGroup"]["description"] == "Greet multiple people at once."


def test_mcp_tool_call():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", _initialize_params()),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "greet.v1.GreetService.Greet",
                    "arguments": {"name": "World"},
                },
            ),
        ]
    )
    content = responses[1]["result"]["content"]
    assert len(content) == 1
    result = json.loads(content[0]["text"])
    assert result["message"] == "Hi World"


async def test_mcp_tool_call_uses_canonical_proto_json_names(server):
    from invariant.projections.mcp import mcp_dispatch

    response = await mcp_dispatch(
        server,
        {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "tools/call",
            "params": {
                "name": "greet.v1.GreetService.Greet",
                "arguments": {"name": "Canonical", "wireSequenceId": "9007199254740993"},
            },
        },
    )

    result = json.loads(response["result"]["content"][0]["text"])
    assert result == {
        "message": "Hi Canonical",
        "wireDisplayLabel": "Canonical",
        "wireResponseCount": "9007199254740993",
    }


def test_mcp_tool_call_rejects_unknown_field():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", _initialize_params()),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "greet.v1.GreetService.Greet",
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
            _mcp_request(0, "initialize", _initialize_params()),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "greet.v1.GreetService.Greet",
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
            _mcp_request(0, "initialize", _initialize_params()),
            _mcp_request(
                1,
                "tools/call",
                {
                    "name": "greet.v1.GreetService.GreetGroup",
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
            _mcp_request(0, "initialize", _initialize_params()),
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
            _mcp_request(0, "initialize", _initialize_params()),
            _mcp_request(1, "ping", {}),
        ]
    )
    assert responses[1]["result"] == {}


def test_mcp_unknown_method():
    responses = _run_mcp_session(
        [
            _mcp_request(0, "initialize", _initialize_params()),
            _mcp_request(1, "unknown/method", {}),
        ]
    )
    assert "error" in responses[1]
    assert responses[1]["error"]["code"] == -32601


def test_mcp_stdio_rejects_invalid_request_shapes_and_continues():
    invalid_messages = [
        "[]",
        json.dumps({"jsonrpc": "1.0", "id": 1, "method": "ping"}),
        json.dumps({"jsonrpc": "2.0", "id": 2, "method": 7}),
        json.dumps({"jsonrpc": "2.0", "id": None, "method": "ping"}),
        json.dumps({"jsonrpc": "2.0", "id": True, "method": "ping"}),
        json.dumps({"jsonrpc": "2.0", "id": 1.0, "method": "ping"}),
        json.dumps({"jsonrpc": "2.0", "id": 9007199254740992, "method": "ping"}),
        json.dumps({"jsonrpc": "2.0", "id": -9007199254740992, "method": "ping"}),
    ]
    notification = json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"})
    client_response = json.dumps({"jsonrpc": "2.0", "id": 7, "result": {}})
    responses = _run_mcp_session(
        [
            *invalid_messages,
            notification,
            client_response,
            _mcp_request(3, "ping", {}),
        ]
    )

    assert responses[:-1] == [
        {
            "jsonrpc": "2.0",
            "id": None,
            "error": {"code": -32600, "message": "Invalid Request"},
        }
    ] * len(invalid_messages)
    assert responses[-1]["result"] == {}


def test_mcp_stdio_uses_strict_json_and_exact_client_response_shapes():
    valid_client_responses = [
        json.dumps({"jsonrpc": "2.0", "id": 7, "result": {}}),
        json.dumps({"jsonrpc": "2.0", "error": {"code": -32000, "message": "ignored"}}),
        json.dumps({"jsonrpc": "2.0", "id": "call-8", "error": {"code": -32001, "message": "ignored"}}),
    ]
    invalid_client_responses = [
        json.dumps({"jsonrpc": "2.0", "id": None, "result": {}}),
        json.dumps({"jsonrpc": "2.0", "id": 9, "result": []}),
        json.dumps({"jsonrpc": "2.0", "id": 9007199254740992, "result": {}}),
        json.dumps({"jsonrpc": "2.0", "error": {"code": True, "message": "invalid"}}),
    ]
    responses = _run_mcp_session(
        [
            '{"jsonrpc":"2.0","id":1,"method":"ping","params":{"value":NaN}}',
            '{"jsonrpc":"2.0","id":2,"method":"ping","params":{"value":Infinity}}',
            *valid_client_responses,
            *invalid_client_responses,
            _mcp_request(10, "ping", {}),
        ]
    )

    assert [response["error"]["code"] for response in responses[:-1]] == [
        -32700,
        -32700,
        -32600,
        -32600,
        -32600,
        -32600,
    ]
    assert responses[-1] == {"jsonrpc": "2.0", "id": 10, "result": {}}


def test_mcp_numeric_ids_use_the_portable_safe_integer_range():
    from invariant.projections.mcp import _id_key, _valid_jsonrpc_id

    assert _valid_jsonrpc_id(9007199254740991)
    assert _valid_jsonrpc_id(-9007199254740991)
    assert not _valid_jsonrpc_id(9007199254740992)
    assert not _valid_jsonrpc_id(-9007199254740992)
    assert not _valid_jsonrpc_id(1.0)
    assert _id_key(-0) == _id_key(0) == "int:0"

    responses = _run_mcp_session(
        [
            '{"jsonrpc":"2.0","id":9007199254740991,"method":"ping"}',
            '{"jsonrpc":"2.0","id":-9007199254740991,"method":"ping"}',
            '{"jsonrpc":"2.0","id":-0,"method":"ping"}',
        ]
    )
    assert [response["id"] for response in responses] == [9007199254740991, -9007199254740991, 0]


def test_mcp_stdio_rejects_invalid_method_params_and_ignores_malformed_cancellation():
    malformed_cancellation = json.dumps(
        {
            "jsonrpc": "2.0",
            "method": "notifications/cancelled",
            "params": [],
        }
    )
    responses = _run_mcp_session(
        [
            _mcp_request(1, "tools/call", []),
            _mcp_request(2, "tools/call", {"name": [], "arguments": {}}),
            _mcp_request(3, "tools/call", {"name": "greet.v1.GreetService.Greet", "arguments": []}),
            malformed_cancellation,
            _mcp_request(4, "ping", []),
            _mcp_request(5, "ping", {}),
        ]
    )

    responses_by_id = {response["id"]: response for response in responses}
    assert [responses_by_id[msg_id]["error"]["code"] for msg_id in (1, 2, 3, 4)] == [
        -32602,
        -32602,
        -32602,
        -32602,
    ]
    assert responses_by_id[5]["result"] == {}


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
        reader.feed_data(
            b'{"jsonrpc":"2.0","id":-0,"method":"tools/call",'
            b'"params":{"name":"greet.v1.GreetService.Greet","arguments":{"name":"blocked"}}}\n'
        )
        await asyncio.wait_for(started.wait(), timeout=2)

        cancellation = {
            "jsonrpc": "2.0",
            "method": "notifications/cancelled",
            "params": {"requestId": 0, "reason": "client no longer needs it"},
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


async def test_mcp_stdio_releases_completed_tasks_before_eof(monkeypatch):
    from invariant.projections import mcp

    class ImmediateServicer:
        async def Greet(self, request, context):
            del context
            return greet_pb2.GreetResponse(message=f"Hi {request.name}")

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(server, ImmediateServicer())
    reader = asyncio.StreamReader()
    responses: list[dict] = []
    written: asyncio.Queue[dict] = asyncio.Queue()

    async def stdin_reader():
        return reader

    def write_response(response):
        responses.append(response)
        written.put_nowait(response)

    monkeypatch.setattr(mcp, "_stdin_reader", stdin_reader)
    monkeypatch.setattr(mcp, "_write_response", write_response)

    session = mcp._StdioMCP(server)
    runner = asyncio.create_task(session.run())
    try:
        call = {
            "jsonrpc": "2.0",
            "id": "completed-call",
            "method": "tools/call",
            "params": {"name": "greet.v1.GreetService.Greet", "arguments": {"name": "released"}},
        }
        reader.feed_data((json.dumps(call) + "\n").encode())

        first = await asyncio.wait_for(written.get(), timeout=2)
        await asyncio.sleep(0)

        assert first["id"] == "completed-call"
        assert session._inflight == {}
        assert session._background == set()
        assert not runner.done()

        reader.feed_data((_mcp_request("next-request", "ping") + "\n").encode())
        second = await asyncio.wait_for(written.get(), timeout=2)
        assert second == {"jsonrpc": "2.0", "id": "next-request", "result": {}}
        assert len(responses) == 2
        assert not runner.done()

        reader.feed_eof()
        await asyncio.wait_for(runner, timeout=2)
    finally:
        if not runner.done():
            runner.cancel()
            await asyncio.gather(runner, return_exceptions=True)
        await server.stop(grace=0)


async def test_mcp_cancellation_rejects_out_of_range_numeric_request_id():
    from invariant.projections import mcp

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    session = mcp._StdioMCP(server)
    task = asyncio.create_task(asyncio.Event().wait())
    session._inflight[mcp._id_key(9007199254740991)] = task
    try:
        session._handle_notification(
            {
                "jsonrpc": "2.0",
                "method": "notifications/cancelled",
                "params": {"requestId": 9007199254740992},
            }
        )
        await asyncio.sleep(0)
        assert not task.cancelling()

        session._handle_notification(
            {
                "jsonrpc": "2.0",
                "method": "notifications/cancelled",
                "params": {"requestId": 9007199254740991},
            }
        )
        await asyncio.sleep(0)
        assert task.cancelling()
    finally:
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
        await server.stop(grace=0)


async def test_mcp_stdio_cancel_suppresses_response_when_handler_swallows_cancellation(monkeypatch):
    from invariant.projections import mcp

    started = asyncio.Event()
    swallowed = asyncio.Event()

    class CancellationSwallowingServicer:
        async def Greet(self, request, context):
            del request, context
            started.set()
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                swallowed.set()
                return greet_pb2.GreetResponse(message="too late")

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(server, CancellationSwallowingServicer())
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
            "params": {"name": "greet.v1.GreetService.Greet", "arguments": {"name": "blocked"}},
        }
        reader.feed_data((json.dumps(call) + "\n").encode())
        await asyncio.wait_for(started.wait(), timeout=2)
        cancellation = {
            "jsonrpc": "2.0",
            "method": "notifications/cancelled",
            "params": {"requestId": "call-1"},
        }
        reader.feed_data((json.dumps(cancellation) + "\n").encode())
        reader.feed_eof()

        await asyncio.wait_for(runner, timeout=2)
        await asyncio.wait_for(swallowed.wait(), timeout=2)
        assert responses == []
    finally:
        if not runner.done():
            runner.cancel()
            await asyncio.gather(runner, return_exceptions=True)
        await server.stop(grace=0)
