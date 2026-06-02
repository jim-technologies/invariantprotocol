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

        def log_message(self, format, *args):  # noqa: A002
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


def test_http_client_binding_preserves_trailing_slash():
    # APIs like Django REST need the trailing slash (else 301/404).
    _b, url = HTTPClientBinding.new("GET", "/questions/", "").build({}, "https://api.example.com")
    assert url == "https://api.example.com/questions/"
    # ... but a path without one stays without one.
    _b2, url2 = HTTPClientBinding.new("GET", "/questions", "").build({}, "https://api.example.com")
    assert url2 == "https://api.example.com/questions"


def _json_name_request_type():
    """Compile, at runtime, a request message exercising json_name:
    string user_id = 1;                               -> "userId" (default)
    string time_in_force = 2 [json_name="timeInForce"];
    string client_order_id = 3 [json_name="client_order_id"];  # snake on wire
    """
    from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

    fd = descriptor_pb2.FieldDescriptorProto
    fdp = descriptor_pb2.FileDescriptorProto(name="jsonname_orders.proto", syntax="proto3", package="t.v1")
    m = fdp.message_type.add(name="CreateOrderRequest")
    m.field.add(name="user_id", number=1, label=fd.LABEL_OPTIONAL, type=fd.TYPE_STRING)
    f2 = m.field.add(name="time_in_force", number=2, label=fd.LABEL_OPTIONAL, type=fd.TYPE_STRING)
    f2.json_name = "timeInForce"
    f3 = m.field.add(name="client_order_id", number=3, label=fd.LABEL_OPTIONAL, type=fd.TYPE_STRING)
    f3.json_name = "client_order_id"

    pool = descriptor_pool.DescriptorPool()
    pool.Add(fdp)
    desc = pool.FindMessageTypeByName("t.v1.CreateOrderRequest")
    return desc, message_factory.GetMessageClass(desc)


def test_binding_honors_json_name_on_body_with_proto_name_path():
    from google.protobuf import json_format

    desc, order_cls = _json_name_request_type()
    # Spec-correct google.api.http: the path references the PROTO field name.
    binding = HTTPClientBinding.new("POST", "/v1/users/{user_id}/orders", "*")
    binding.resolve_fields(desc)

    msg = order_cls(user_id="U1", time_in_force="GTC", client_order_id="C1")
    args = json_format.MessageToDict(msg)  # default mapping, as the handler does
    body, url = binding.build(args, "https://api.example.com")

    # proto-name path template still resolves (value in the URL, key consumed):
    assert url == "https://api.example.com/v1/users/U1/orders"
    # body keys honor json_name (explicit camelCase AND explicit snake_case):
    assert json.loads(body) == {"timeInForce": "GTC", "client_order_id": "C1"}


def test_binding_honors_json_name_on_query():
    from google.protobuf import json_format

    desc, order_cls = _json_name_request_type()
    binding = HTTPClientBinding.new("GET", "/v1/users/{user_id}/orders", "")
    binding.resolve_fields(desc)

    msg = order_cls(user_id="U1", time_in_force="GTC", client_order_id="C1")
    args = json_format.MessageToDict(msg)
    body, url = binding.build(args, "https://api.example.com")

    assert body is None
    params = urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)
    assert params["timeInForce"] == ["GTC"]
    assert params["client_order_id"] == ["C1"]
    assert "time_in_force" not in params  # proto name never hits the wire


async def test_connect_http_response_body_mapping():
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

        def log_message(self, format, *args):  # noqa: A002
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
        try:
            resp = await handler(greet_pb2.GreetRequest(name="World"), None)
            assert resp.message == "Hello, World"
        finally:
            await handler.aclose()
    finally:
        httpd.shutdown()


async def test_connect_http_response_observer_captures_raw_bytes():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        seen: dict = {}

        def observer(resp):
            seen["method_path"] = resp.method_path
            seen["status"] = resp.status_code
            seen["body"] = resp.body  # raw, undecoded bytes

        srv.use_http_response_observer(observer)  # must precede connect_http
        srv.connect_http(f"http://localhost:{port}")
        try:
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            # the observer saw the verbatim response bytes, before parsing
            assert seen["status"] == 200
            assert isinstance(seen["body"], bytes)
            assert json.loads(seen["body"]) == {"message": "Hello, World"}
            assert seen["method_path"].endswith("/Greet")
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_query_provider_adds_auth_params():
    captured: dict = {}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            captured["path"] = self.path  # includes the query string
            raw = json.dumps({"message": "Hello, World"}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        # e.g. an API key (The Odds API) or HMAC signature + timestamp (Binance)
        srv.use_http_query_provider(lambda _req: {"apiKey": "secret", "v": "2"})
        srv.connect_http(f"http://localhost:{port}")
        try:
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert "apiKey=secret" in captured["path"]
            assert "v=2" in captured["path"]
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_httpbody_response_returns_raw_bytes():
    import google.api.httpbody_pb2 as hb  # noqa: F401  (registers HttpBody)

    payload = b'{"weird": [1, 2, 3], "ok": true}'  # arbitrary, unmodeled body

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *_a):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        handler = HTTPDynamicHandler(
            base_url=f"http://localhost:{port}",
            binding=HTTPClientBinding.new("GET", "/raw", ""),
            output_type="google.api.HttpBody",
            timeout=5.0,
            method_path="/x.Svc/Raw",
        )
        try:
            resp = await handler(greet_pb2.GreetRequest(name="x"), None)
            # raw bytes verbatim — no JSON->proto parsing
            assert resp.data == payload
            assert resp.content_type == "application/json"
        finally:
            await handler.aclose()
    finally:
        httpd.shutdown()


async def test_connect_http_httpbody_request_sends_raw_body():
    import google.api.httpbody_pb2 as hb

    captured: dict = {}

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("Content-Length", "0"))
            captured["body"] = self.rfile.read(n) if n else b""
            captured["content_type"] = self.headers.get("Content-Type")
            raw = b"{}"
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_a):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        handler = HTTPDynamicHandler(
            base_url=f"http://localhost:{port}",
            binding=HTTPClientBinding.new("POST", "/upload", "*"),
            output_type="greet.v1.GreetResponse",
            timeout=5.0,
            method_path="/x.Svc/Upload",
            input_type="google.api.HttpBody",
        )
        try:
            await handler(hb.HttpBody(content_type="text/csv", data=b"a,b\n1,2"), None)
            assert captured["body"] == b"a,b\n1,2"
            assert captured["content_type"] == "text/csv"
        finally:
            await handler.aclose()
    finally:
        httpd.shutdown()


async def test_connect_http_registers_tools():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            assert set(srv.tools.keys()) == {"GreetService.Greet", "GreetService.GreetGroup"}
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_cli_greet():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_cli_greet_group():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = await srv._cli(
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
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_maps_remote_error():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            with pytest.raises(InvariantError, match="bad name") as exc:
                await srv._cli(["GreetService", "Greet", "-r", '{"name":"bad"}'])
            assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
            assert exc.value.to_payload()["code"] == "invalid_argument"
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_unknown_service():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    try:
        with pytest.raises(ValueError, match="not found"):
            srv.connect_http("http://localhost:1", service_name="does.not.ExistService")
    finally:
        await srv.stop()


async def test_connect_http_injects_headers_from_env(monkeypatch):
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

        def log_message(self, format, *args):  # noqa: A002
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_sets_default_user_agent(monkeypatch):
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

        def log_message(self, format, *args):  # noqa: A002
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert seen["user_agent"] is not None
            assert seen["user_agent"].startswith("invariant-protocol/")
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_user_agent_override_from_env(monkeypatch):
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

        def log_message(self, format, *args):  # noqa: A002
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert seen["user_agent"] == "custom-agent/9.9"
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_retries_transient_get():
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

        def log_message(self, format, *args):  # noqa: A002
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()

    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert Handler.attempts == 3
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_does_not_retry_post():
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

        def log_message(self, format, *args):  # noqa: A002
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()

    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["GreetService", "GreetGroup", "-r", '{"people":[{"name":"Alice"}]}'])
            assert exc.value.code == grpc.StatusCode.UNAVAILABLE
            assert Handler.attempts == 1
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_uses_dynamic_header_provider():
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

        def log_message(self, format, *args):  # noqa: A002
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
            result = await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert seen["method_path"] == "/greet.v1.GreetService/Greet"
            assert seen["method"] == "GET"
            assert seen["body"] == b""
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_dynamic_header_provider_error():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)

    def provider(_req):
        raise RuntimeError("missing signing key")

    srv.use_http_header_provider(provider)
    srv.connect_http("http://localhost:1")
    try:
        with pytest.raises(InvariantError, match="missing signing key") as exc:
            await srv._cli(["GreetService", "Greet", "-r", '{"name":"World"}'])
        assert exc.value.code == grpc.StatusCode.UNAUTHENTICATED
    finally:
        await srv.stop()
