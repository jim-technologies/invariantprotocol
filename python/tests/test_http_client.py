"""Test descriptor-driven HTTP client (Server.connect_http)."""

from __future__ import annotations

import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import grpc
import pytest
from conftest import DESCRIPTOR_PATH

from invariant import InvariantError, Server


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
