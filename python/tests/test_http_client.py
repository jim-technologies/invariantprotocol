"""Test descriptor-driven HTTP client (Server.connect_http)."""

from __future__ import annotations

import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import greet_pb2
import grpc
import pytest
from conftest import DESCRIPTOR_PATH

from invariant import InvariantError, Server
from invariant.http_client import HTTPClientBinding, HTTPDynamicHandler


def _start_annotated_http_backend() -> tuple[ThreadingHTTPServer, int]:
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            parsed = urllib.parse.urlsplit(self.path)
            if not parsed.path.startswith("/v1/greet/"):
                self.send_response(404)
                self.end_headers()
                return

            name = urllib.parse.unquote(parsed.path.removeprefix("/v1/greet/"))
            if name == "bad":
                self._write_json(
                    400,
                    {
                        "error": {
                            "code": "INVALID_ARGUMENT",
                            "message": "bad name",
                        }
                    },
                )
                return

            self._write_json(200, {"message": f"Hello, {name}"})

        def do_POST(self):
            parsed = urllib.parse.urlsplit(self.path)
            if parsed.path != "/v1/greet:group":
                self.send_response(404)
                self.end_headers()
                return

            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length) if length > 0 else b"{}"
            data = json.loads(body.decode())
            people = data.get("people", [])
            messages = [f"Hello, {p['name']}" for p in people]
            self._write_json(200, {"messages": messages, "count": len(messages)})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    return httpd, port


def _connect_http_server(base_url: str) -> Server:
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http(base_url)
    return srv


def test_http_client_binding_flattens_query_wrapper():
    binding = HTTPClientBinding.new("GET", "/v1/item/{id}", "")
    body, url = binding.build(
        {
            "id": 42,
            "query": {"limit": 5, "filters": {"hero_id": 1}},
        },
        "https://api.example.com",
    )
    parsed = urllib.parse.urlsplit(url)
    params = urllib.parse.parse_qs(parsed.query)

    assert body is None
    assert params["limit"] == ["5"]
    assert params["filters.hero_id"] == ["1"]
    assert "query.limit" not in params


def test_http_client_binding_query_wrapper_does_not_override_explicit_fields():
    binding = HTTPClientBinding.new("GET", "/v1/item/{id}", "")
    _body, url = binding.build(
        {
            "id": 42,
            "limit": 3,
            "query": {"limit": 5},
        },
        "https://api.example.com",
    )
    parsed = urllib.parse.urlsplit(url)
    params = urllib.parse.parse_qs(parsed.query)
    assert params["limit"] == ["3"]


def test_connect_http_response_body_mapping():
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path != "/v1/greet/World":
                self.send_response(404)
                self.end_headers()
                return
            raw = json.dumps("Hello, World").encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()

    try:
        handler = HTTPDynamicHandler(
            base_url=f"http://localhost:{port}",
            binding=HTTPClientBinding.new("GET", "/v1/greet/{name}", "", response_body="message"),
            output_type="greet.v1.GreetResponse",
            timeout=5.0,
            method_path="/greet.v1.GreetService/Greet",
        )
        resp = handler(greet_pb2.GreetRequest(name="World"), None)
        assert resp.message == "Hello, World"
    finally:
        httpd.shutdown()


def test_connect_http_registers_tools():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            assert set(srv.tools.keys()) == {"GreetService.Greet", "GreetService.GreetGroup"}
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_cli_greet():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_cli_greet_group():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = srv._cli(
                [
                    "GreetService",
                    "GreetGroup",
                    "-r",
                    '{"people":[{"name":"Alice"},{"name":"Bob"}]}',
                ]
            )
            assert result["messages"] == ["Hello, Alice", "Hello, Bob"]
            assert result["count"] == 2
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_maps_remote_error():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            with pytest.raises(InvariantError, match="bad name") as exc:
                srv._cli(["GreetService", "Greet", "-r", '{"name":"bad"}'])
            assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
            assert exc.value.to_payload()["code"] == "INVALID_ARGUMENT"
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_unknown_service():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    try:
        with pytest.raises(ValueError, match="not found"):
            srv.connect_http("http://localhost:1", service_name="does.not.ExistService")
    finally:
        srv.stop()


def test_connect_http_injects_headers_from_env(monkeypatch):
    monkeypatch.setenv("INVARIANT_HTTP_HEADER_AUTHORIZATION", "Bearer test-token")

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path != "/v1/greet/World":
                self.send_response(404)
                self.end_headers()
                return
            if self.headers.get("Authorization") != "Bearer test-token":
                self._write_json(401, {"error": {"code": "UNAUTHENTICATED", "message": "missing auth"}})
                return
            self._write_json(200, {"message": "Hello, World"})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_sets_default_user_agent(monkeypatch):
    monkeypatch.delenv("INVARIANT_HTTP_HEADER_USER_AGENT", raising=False)
    seen: dict[str, str | None] = {}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            seen["user_agent"] = self.headers.get("User-Agent")
            if self.path != "/v1/greet/World":
                self.send_response(404)
                self.end_headers()
                return
            self._write_json(200, {"message": "Hello, World"})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert seen["user_agent"] is not None
            assert seen["user_agent"].startswith("invariant-protocol/")
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_user_agent_override_from_env(monkeypatch):
    monkeypatch.setenv("INVARIANT_HTTP_HEADER_USER_AGENT", "custom-agent/9.9")
    seen: dict[str, str | None] = {}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            seen["user_agent"] = self.headers.get("User-Agent")
            if self.path != "/v1/greet/World":
                self.send_response(404)
                self.end_headers()
                return
            self._write_json(200, {"message": "Hello, World"})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert seen["user_agent"] == "custom-agent/9.9"
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_retries_transient_get():
    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_GET(self):
            if self.path != "/v1/greet/World":
                self.send_response(404)
                self.end_headers()
                return

            type(self).attempts += 1
            if type(self).attempts <= 2:
                self._write_json(
                    503,
                    {
                        "error": {
                            "code": "UNAVAILABLE",
                            "message": "temporary outage",
                        }
                    },
                    extra_headers={"Retry-After": "0"},
                )
                return
            self._write_json(200, {"message": "Hello, World"})

        def _write_json(self, status: int, payload: dict, *, extra_headers: dict[str, str] | None = None):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            if extra_headers:
                for k, v in extra_headers.items():
                    self.send_header(k, v)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()

    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert Handler.attempts == 3
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_does_not_retry_post():
    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_POST(self):
            if self.path != "/v1/greet:group":
                self.send_response(404)
                self.end_headers()
                return
            type(self).attempts += 1
            self._write_json(
                503,
                {
                    "error": {
                        "code": "UNAVAILABLE",
                        "message": "temporary outage",
                    }
                },
            )

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()

    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            with pytest.raises(InvariantError) as exc:
                srv._cli(["GreetService", "GreetGroup", "-r", '{"people":[{"name":"Alice"}]}'])
            assert exc.value.code == grpc.StatusCode.UNAVAILABLE
            assert Handler.attempts == 1
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_uses_dynamic_header_provider():
    seen = {}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path != "/v1/greet/World":
                self.send_response(404)
                self.end_headers()
                return
            if self.headers.get("X-Signature") != "sig-value":
                self._write_json(401, {"error": {"code": "UNAUTHENTICATED", "message": "missing signature"}})
                return
            self._write_json(200, {"message": "Hello, World"})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format, *args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()

    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)

        def provider(req):
            seen["method_path"] = req.method_path
            seen["method"] = req.method
            seen["body"] = req.body
            return {"X-Signature": "sig-value"}

        srv.use_http_header_provider(provider)
        srv.connect_http(f"http://localhost:{port}")
        try:
            result = srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert seen["method_path"] == "/greet.v1.GreetService/Greet"
            assert seen["method"] == "GET"
            assert seen["body"] == b""
        finally:
            srv.stop()
    finally:
        httpd.shutdown()


def test_connect_http_dynamic_header_provider_error():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)

    def provider(_req):
        raise RuntimeError("missing signing key")

    srv.use_http_header_provider(provider)
    srv.connect_http("http://localhost:1")
    try:
        with pytest.raises(InvariantError, match="missing signing key") as exc:
            srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
        assert exc.value.code == grpc.StatusCode.UNAUTHENTICATED
    finally:
        srv.stop()
