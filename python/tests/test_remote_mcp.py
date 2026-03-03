"""Test remote gRPC proxy mode (Server.connect)."""

import json
import os
import subprocess
import sys

import greet_pb2

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")


def test_connect_registers_tools(server):
    """connect() should register the same tools as register()."""
    port = server._start_grpc(port=0)
    try:
        from invariant import Server

        remote = Server.from_descriptor(DESCRIPTOR_PATH)
        remote.connect(f"localhost:{port}")

        assert "GreetService.Greet" in remote.tools
        assert "GreetService.GreetGroup" in remote.tools
        assert len(remote.tools) == 2

        # Schema should match
        local_schema = server.tools["GreetService.Greet"].input_schema
        remote_schema = remote.tools["GreetService.Greet"].input_schema
        assert local_schema == remote_schema
    finally:
        remote.stop()
        server._stop_grpc()


def test_connect_greet(server):
    """Remote tool call should proxy to the gRPC server and return the correct response."""
    port = server._start_grpc(port=0)
    try:
        from invariant import Server

        remote = Server.from_descriptor(DESCRIPTOR_PATH)
        remote.connect(f"localhost:{port}")

        req = greet_pb2.GreetRequest(name="World")
        resp = remote.tools["GreetService.Greet"].handler(req, None)
        assert resp.message == "Hi World"
    finally:
        remote.stop()
        server._stop_grpc()


def test_connect_greet_with_enum_and_tags(server):
    """Remote tool call should handle enums and maps correctly."""
    port = server._start_grpc(port=0)
    try:
        from invariant import Server

        remote = Server.from_descriptor(DESCRIPTOR_PATH)
        remote.connect(f"localhost:{port}")

        req = greet_pb2.GreetRequest(
            name="World",
            mood=greet_pb2.MOOD_HAPPY,
            tags={"lang": "en"},
        )
        resp = remote.tools["GreetService.Greet"].handler(req, None)
        assert resp.message == "Hi World"
        assert resp.mood == greet_pb2.MOOD_HAPPY
        assert resp.tags["lang"] == "en"
    finally:
        remote.stop()
        server._stop_grpc()


def test_connect_greet_group(server):
    """Remote GreetGroup should work with repeated message fields."""
    port = server._start_grpc(port=0)
    try:
        from invariant import Server

        remote = Server.from_descriptor(DESCRIPTOR_PATH)
        remote.connect(f"localhost:{port}")

        req = greet_pb2.GreetGroupRequest(
            people=[
                greet_pb2.Person(name="Alice", mood=greet_pb2.MOOD_HAPPY),
                greet_pb2.Person(name="Bob", mood=greet_pb2.MOOD_SAD),
            ]
        )
        resp = remote.tools["GreetService.GreetGroup"].handler(req, None)
        assert list(resp.messages) == ["Hi Alice", "Hi Bob"]
        assert resp.count == 2
    finally:
        remote.stop()
        server._stop_grpc()


def _mcp_request(msg_id, method, params=None):
    msg = {"jsonrpc": "2.0", "id": msg_id, "method": method}
    if params is not None:
        msg["params"] = params
    return json.dumps(msg)


def _run_remote_mcp_session(grpc_port: int, messages: list[str]) -> list[dict]:
    """Send JSON-RPC messages to a remote-mode MCP server via stdio."""
    stdin_data = "\n".join(messages) + "\n"

    test_dir = os.path.dirname(os.path.abspath(__file__))
    src_dir = os.path.join(test_dir, "..", "src")
    gen_dir = os.path.join(test_dir, "proto", "gen")
    descriptor = os.path.join(test_dir, "proto", "descriptor.binpb")

    script = f"""
import sys
sys.path.insert(0, {src_dir!r})
sys.path.insert(0, {gen_dir!r})
import greet_pb2
from invariant import Server

server = Server.from_descriptor({descriptor!r})
server.connect("localhost:{grpc_port}")
server.serve(mcp=True)
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


def test_remote_mcp_tools_list(server):
    """Remote MCP should list the same tools as local MCP."""
    port = server._start_grpc(port=0)
    try:
        responses = _run_remote_mcp_session(
            port,
            [
                _mcp_request(0, "initialize", {}),
                _mcp_request(1, "tools/list", {}),
            ],
        )
        tools = responses[1]["result"]["tools"]
        assert len(tools) == 2
        tools_by_name = {t["name"]: t for t in tools}
        assert "GreetService.Greet" in tools_by_name
        assert "GreetService.GreetGroup" in tools_by_name
    finally:
        server._stop_grpc()


def test_remote_mcp_tool_call(server):
    """Remote MCP tool call should proxy to the gRPC server."""
    port = server._start_grpc(port=0)
    try:
        responses = _run_remote_mcp_session(
            port,
            [
                _mcp_request(0, "initialize", {}),
                _mcp_request(
                    1,
                    "tools/call",
                    {"name": "GreetService.Greet", "arguments": {"name": "Remote"}},
                ),
            ],
        )
        result = json.loads(responses[1]["result"]["content"][0]["text"])
        assert result["message"] == "Hi Remote"
    finally:
        server._stop_grpc()


def test_remote_mcp_tool_call_with_enum(server):
    """Remote MCP tool call should handle enums and maps."""
    port = server._start_grpc(port=0)
    try:
        responses = _run_remote_mcp_session(
            port,
            [
                _mcp_request(0, "initialize", {}),
                _mcp_request(
                    1,
                    "tools/call",
                    {
                        "name": "GreetService.Greet",
                        "arguments": {
                            "name": "World",
                            "mood": "MOOD_HAPPY",
                            "tags": {"lang": "en"},
                        },
                    },
                ),
            ],
        )
        result = json.loads(responses[1]["result"]["content"][0]["text"])
        assert result["message"] == "Hi World"
        assert result["mood"] == "MOOD_HAPPY"
        assert result["tags"]["lang"] == "en"
    finally:
        server._stop_grpc()
