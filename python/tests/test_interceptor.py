"""Test interceptor middleware across all projections."""

import json
import os
import subprocess
import sys
import urllib.request

import pytest


def test_interceptor_fires_on_cli(server):
    log = []

    def interceptor(request, context, info, handler):
        log.append("A-before")
        resp = handler(request, context)
        log.append("A-after")
        return resp

    server.use(interceptor)
    try:
        result = server._cli(["GreetService", "Greet", "-r", '{"name": "CLI"}'])
        assert result["message"] == "Hi CLI"
        assert log == ["A-before", "A-after"]
    finally:
        server._interceptors.clear()


def test_interceptor_fires_on_http(server):
    log = []

    def interceptor(request, context, info, handler):
        log.append("A-before")
        resp = handler(request, context)
        log.append("A-after")
        return resp

    server.use(interceptor)
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/Greet",
            data=json.dumps({"name": "HTTP"}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["message"] == "Hi HTTP"
        assert log == ["A-before", "A-after"]
    finally:
        server._stop_http()
        server._interceptors.clear()


def test_interceptor_fires_on_grpc(server):
    log = []

    def interceptor(request, context, info, handler):
        log.append("A-before")
        resp = handler(request, context)
        log.append("A-after")
        return resp

    server.use(interceptor)
    port = server._start_grpc(port=0)
    try:
        import grpc
        from google.protobuf import descriptor_pool, message_factory

        pool = descriptor_pool.Default()
        req_desc = pool.FindMessageTypeByName("greet.v1.GreetRequest")
        resp_desc = pool.FindMessageTypeByName("greet.v1.GreetResponse")
        req_class = message_factory.GetMessageClass(req_desc)
        resp_class = message_factory.GetMessageClass(resp_desc)

        channel = grpc.insecure_channel(f"localhost:{port}")
        stub = channel.unary_unary(
            "/greet.v1.GreetService/Greet",
            request_serializer=lambda msg: msg.SerializeToString(),
            response_deserializer=resp_class.FromString,
        )

        request = req_class()
        request.name = "gRPC"
        response = stub(request)
        assert response.message == "Hi gRPC"
        assert log == ["A-before", "A-after"]
        channel.close()
    finally:
        server._stop_grpc()
        server._interceptors.clear()


def test_interceptor_chain_order(server):
    log = []

    def make_interceptor(label):
        def interceptor(request, context, info, handler):
            log.append(f"{label}-before")
            resp = handler(request, context)
            log.append(f"{label}-after")
            return resp
        return interceptor

    server.use(make_interceptor("A"))
    server.use(make_interceptor("B"))
    try:
        result = server._cli(["GreetService", "Greet", "-r", '{"name": "Order"}'])
        assert result["message"] == "Hi Order"
        assert log == ["A-before", "B-before", "B-after", "A-after"]
    finally:
        server._interceptors.clear()


def test_interceptor_short_circuit(server):
    def blocking_interceptor(request, context, info, handler):
        raise ValueError("blocked by interceptor")

    server.use(blocking_interceptor)
    try:
        with pytest.raises(ValueError, match="blocked by interceptor"):
            server._cli(["GreetService", "Greet", "-r", '{"name": "Blocked"}'])
    finally:
        server._interceptors.clear()


def test_interceptor_full_method(server):
    captured = {}

    def interceptor(request, context, info, handler):
        captured["full_method"] = info.full_method
        return handler(request, context)

    server.use(interceptor)
    try:
        result = server._cli(["GreetService", "Greet", "-r", '{"name": "Method"}'])
        assert result["message"] == "Hi Method"
        assert captured["full_method"] == "/greet.v1.GreetService/Greet"
    finally:
        server._interceptors.clear()


def test_no_interceptors_backward_compat(server):
    # No interceptors registered — should work as before.
    assert len(server._interceptors) == 0
    result = server._cli(["GreetService", "Greet", "-r", '{"name": "Compat"}'])
    assert result["message"] == "Hi Compat"


def test_interceptor_fires_on_mcp():
    """Test interceptor fires on MCP projection via subprocess."""
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

log = []

def interceptor(request, context, info, handler):
    log.append("A-before")
    resp = handler(request, context)
    log.append("A-after")
    return resp

class GreetServicer:
    def Greet(self, request, context):
        return greet_pb2.GreetResponse(message=f"Hi {{request.name}}")
    def GreetGroup(self, request, context):
        return greet_pb2.GreetGroupResponse(messages=[], count=0)

server = Server.from_descriptor({descriptor!r})
server.register(GreetServicer())
server.use(interceptor)
server.serve(mcp=True)

# Print log to stderr so we can check it
import sys
print(",".join(log), file=sys.stderr)
"""

    msg = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {
            "name": "GreetService.Greet",
            "arguments": {"name": "MCP"},
        },
    }
    stdin_data = json.dumps(msg) + "\n"

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

    assert len(responses) == 1
    content = responses[0]["result"]["content"]
    result = json.loads(content[0]["text"])
    assert result["message"] == "Hi MCP"

    # Check interceptor log from stderr
    assert "A-before,A-after" in proc.stderr
