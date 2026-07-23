"""Test descriptor-driven HTTP client (Server.connect_http)."""

from __future__ import annotations

import base64
import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import greet_pb2
import greet_pb2_grpc
import grpc
import pytest
from conftest import DESCRIPTOR_PATH
from google.protobuf import descriptor_pb2

from invariant import ChannelOptions, HTTPAuth, InvariantError, Server
from invariant.http_client import HTTPClientBinding, HTTPConnection, HTTPDynamicHandler
from invariant.server import _build_descriptor_pool


def _isolated_descriptor_pool(*generated_files):
    with open(DESCRIPTOR_PATH, "rb") as descriptor_file:
        fds = descriptor_pb2.FileDescriptorSet.FromString(descriptor_file.read())
    known = {file.name for file in fds.file}

    def include(file_descriptor):
        for dependency in file_descriptor.dependencies:
            include(dependency)
        if file_descriptor.name not in known:
            fds.file.add().ParseFromString(file_descriptor.serialized_pb)
            known.add(file_descriptor.name)

    for generated_file in generated_files:
        include(generated_file)
    return _build_descriptor_pool(fds)


def test_http_dynamic_handler_requires_explicit_descriptor_pool():
    connection = HTTPConnection(base_url="http://localhost:1")
    with pytest.raises(TypeError, match="pool"):
        HTTPDynamicHandler(  # type: ignore[call-arg]
            connection=connection,
            binding=HTTPClientBinding.new("GET", "/v1/greet/{name}", ""),
            output_type="greet.v1.GreetResponse",
            method_path="/greet.v1.GreetService/Greet",
        )


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
                        "code": "invalid_argument",
                        "message": "bad name",
                    },
                )
                return
            if name == "cancel":
                self._write_json(499, {"code": "canceled", "message": "request canceled"})
                return
            if name == "wrapped":
                self._write_json(400, {"error": {"code": "INVALID_ARGUMENT", "message": "old shape"}})
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


async def test_shared_standard_interceptor_wraps_connect_http_projection():
    seen: list[tuple[str, str]] = []

    class SharedInterceptor(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            handler = await continuation(handler_call_details)
            assert handler is not None
            terminal = handler.unary_unary
            assert terminal is not None

            async def wrapped(request, context):
                # Remote descriptor-only proxies intentionally use their
                # isolated pool's dynamic class while retaining proto identity.
                seen.append((request.DESCRIPTOR.full_name, handler_call_details.method))
                return await terminal(request, context)

            return grpc.unary_unary_rpc_method_handler(
                wrapped,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

    backend, port = _start_annotated_http_backend()
    server = Server.from_descriptor(DESCRIPTOR_PATH)
    server.use(SharedInterceptor())
    try:
        server.connect_http(f"http://localhost:{port}")
        result = await server._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
        assert result["message"] == "Hello, World"
        assert seen == [("greet.v1.GreetRequest", "/greet.v1.GreetService/Greet")]
    finally:
        await server.stop()
        backend.shutdown()


async def test_connect_http_filters_only_optional_catalog_not_native_grpc_or_reflection():
    from google.protobuf import descriptor_pb2
    from grpc_reflection.v1alpha import reflection_pb2, reflection_pb2_grpc

    backend, port = _start_annotated_http_backend()
    server = Server.from_descriptor(DESCRIPTOR_PATH)
    server.exclude("*.Greet")
    try:
        server.connect_http(f"http://localhost:{port}")
        assert "greet.v1.GreetService.Greet" not in server.tools
        assert "greet.v1.GreetService.GreetGroup" in server.tools

        native = server.grpc_server()
        native_port = native.add_insecure_port("127.0.0.1:0")
        await native.start()
        async with grpc.aio.insecure_channel(f"127.0.0.1:{native_port}") as channel:
            response = await greet_pb2_grpc.GreetServiceStub(channel).Greet(greet_pb2.GreetRequest(name="World"))
            assert response.message == "Hello, World"

            reflection_stub = reflection_pb2_grpc.ServerReflectionStub(channel)

            async def requests():
                yield reflection_pb2.ServerReflectionRequest(file_containing_symbol="greet.v1.GreetService")

            reflection_responses = [response async for response in reflection_stub.ServerReflectionInfo(requests())]

        reflected_files = [
            descriptor_pb2.FileDescriptorProto.FromString(raw)
            for raw in reflection_responses[0].file_descriptor_response.file_descriptor_proto
        ]
        greet_file = next(file for file in reflected_files if file.name == "greet.proto")
        service = next(service for service in greet_file.service if service.name == "GreetService")
        assert [method.name for method in service.method] == ["Greet", "GreetGroup"]
    finally:
        await server.stop()
        backend.shutdown()


def _retry_service_config(
    *,
    max_attempts: int = 3,
    codes: list[str] | None = None,
    service: str = "greet.v1.GreetService",
    method: str | None = None,
    retry_unsafe_methods: bool = False,
) -> dict:
    name = {"service": service}
    if method:
        name["method"] = method
    config = {
        "name": [name],
        "retry_policy": {
            "max_attempts": max_attempts,
            "initial_backoff": "0.1s",
            "max_backoff": "1s",
            "backoff_multiplier": 2.0,
            "retryable_status_codes": codes or ["UNAVAILABLE"],
        },
    }
    if retry_unsafe_methods:
        config["retry_unsafe_methods"] = True
    return {"method_config": [config]}


def _descriptor_with_raw_httpbody() -> bytes:
    from google.api import annotations_pb2, httpbody_pb2
    from google.protobuf import descriptor_pb2

    fds = descriptor_pb2.FileDescriptorSet()
    with open(DESCRIPTOR_PATH, "rb") as f:
        fds.ParseFromString(f.read())

    file_proto = next(f for f in fds.file if f.name == "greet.proto")
    if "google/api/httpbody.proto" not in file_proto.dependency:
        file_proto.dependency.append("google/api/httpbody.proto")
    if not any(file.name == "google/api/httpbody.proto" for file in fds.file):
        httpbody_file = fds.file.add()
        httpbody_pb2.DESCRIPTOR.CopyToProto(httpbody_file)
    svc = next(s for s in file_proto.service if s.name == "GreetService")
    method = svc.method.add(
        name="RawBody",
        input_type=".greet.v1.GreetRequest",
        output_type=".google.api.HttpBody",
    )
    method.options.Extensions[annotations_pb2.http].get = "/raw/{name}"
    return fds.SerializeToString()


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
        conn = HTTPConnection(base_url=f"http://localhost:{port}")
        handler = HTTPDynamicHandler(
            connection=conn,
            binding=HTTPClientBinding.new("GET", "/v1/greet/{name}", "", response_body="message"),
            output_type="greet.v1.GreetResponse",
            method_path="/greet.v1.GreetService/Greet",
            pool=_isolated_descriptor_pool(),
        )
        try:
            resp = await handler(greet_pb2.GreetRequest(name="World"), None)
            assert resp.message == "Hello, World"
        finally:
            await conn.aclose()
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
            seen["duration_ms"] = resp.duration_ms
            seen["success"] = resp.success

        srv.connect_http(f"http://localhost:{port}", observer=observer)
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            # the observer saw the verbatim response bytes, before parsing
            assert seen["status"] == 200
            assert seen["success"] is True
            assert seen["duration_ms"] >= 0
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
        # e.g. an API key or HMAC signature + timestamp.
        srv.connect_http(
            f"http://localhost:{port}",
            auth=HTTPAuth(query_provider=lambda _req: {"apiKey": "secret", "v": "2"}),
        )
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert "apiKey=secret" in captured["path"]
            assert "v=2" in captured["path"]
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_httpbody_response_returns_raw_bytes():
    import google.api.httpbody_pb2 as hb

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
        conn = HTTPConnection(base_url=f"http://localhost:{port}")
        handler = HTTPDynamicHandler(
            connection=conn,
            binding=HTTPClientBinding.new("GET", "/raw", ""),
            output_type="google.api.HttpBody",
            method_path="/x.Svc/Raw",
            pool=_isolated_descriptor_pool(hb.DESCRIPTOR),
        )
        try:
            resp = await handler(greet_pb2.GreetRequest(name="x"), None)
            # raw bytes verbatim — no JSON->proto parsing
            assert resp.data == payload
            assert resp.content_type == "application/json"
        finally:
            await conn.aclose()
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
        conn = HTTPConnection(base_url=f"http://localhost:{port}")
        handler = HTTPDynamicHandler(
            connection=conn,
            binding=HTTPClientBinding.new("POST", "/upload", "*"),
            output_type="greet.v1.GreetResponse",
            method_path="/x.Svc/Upload",
            input_type="google.api.HttpBody",
            pool=_isolated_descriptor_pool(),
        )
        try:
            await handler(hb.HttpBody(content_type="text/csv", data=b"a,b\n1,2"), None)
            assert captured["body"] == b"a,b\n1,2"
            assert captured["content_type"] == "text/csv"
        finally:
            await conn.aclose()
    finally:
        httpd.shutdown()


async def test_connect_http_httpbody_request_without_content_type_sends_no_json_content_type():
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
        conn = HTTPConnection(base_url=f"http://localhost:{port}")
        handler = HTTPDynamicHandler(
            connection=conn,
            binding=HTTPClientBinding.new("POST", "/upload", "*"),
            output_type="greet.v1.GreetResponse",
            method_path="/x.Svc/Upload",
            input_type="google.api.HttpBody",
            pool=_isolated_descriptor_pool(),
        )
        try:
            await handler(hb.HttpBody(data=b"a,b\n1,2"), None)
            assert captured["body"] == b"a,b\n1,2"
            assert captured["content_type"] is None
        finally:
            await conn.aclose()
    finally:
        httpd.shutdown()


async def test_connect_http_registers_tools():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://localhost:{port}", service_config=_retry_service_config())
        try:
            assert set(srv.tools.keys()) == {"greet.v1.GreetService.Greet", "greet.v1.GreetService.GreetGroup"}
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_cli_greet():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://localhost:{port}", service_config=_retry_service_config())
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_cli_greet_group():
    httpd, port = _start_annotated_http_backend()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://localhost:{port}", service_config=_retry_service_config())
        try:
            result = await srv._cli(
                [
                    "greet.v1.GreetService",
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
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://localhost:{port}", service_config=_retry_service_config())
        try:
            with pytest.raises(InvariantError, match="bad name") as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"bad"}'])
            assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
            assert exc.value.to_payload()["code"] == "invalid_argument"

            with pytest.raises(InvariantError, match="request canceled") as canceled:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"cancel"}'])
            assert canceled.value.code == grpc.StatusCode.CANCELLED
            assert canceled.value.to_payload()["code"] == "canceled"

            with pytest.raises(InvariantError, match="HTTP 400") as wrapped:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"wrapped"}'])
            assert wrapped.value.code == grpc.StatusCode.INTERNAL
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_uses_connect_http_status_fallbacks_for_malformed_errors():
    expected = {
        400: grpc.StatusCode.INTERNAL,
        401: grpc.StatusCode.UNAUTHENTICATED,
        403: grpc.StatusCode.PERMISSION_DENIED,
        404: grpc.StatusCode.UNIMPLEMENTED,
        429: grpc.StatusCode.UNAVAILABLE,
        502: grpc.StatusCode.UNAVAILABLE,
        503: grpc.StatusCode.UNAVAILABLE,
        504: grpc.StatusCode.UNAVAILABLE,
        418: grpc.StatusCode.UNKNOWN,
        409: grpc.StatusCode.UNKNOWN,
        499: grpc.StatusCode.UNKNOWN,
        500: grpc.StatusCode.UNKNOWN,
        501: grpc.StatusCode.UNKNOWN,
    }

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            parsed = urllib.parse.urlsplit(self.path)
            status = int(parsed.path.removeprefix("/v1/greet/"))
            body = b"not a Connect error"
            self.send_response(status)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, format, *args):  # noqa: A002
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            for status, code in expected.items():
                with pytest.raises(InvariantError) as exc:
                    await srv._cli(["greet.v1.GreetService", "Greet", "-r", f'{{"name":"{status}"}}'])
                assert exc.value.code == code
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
                self._write_json(401, {"code": "unauthenticated", "message": "missing auth"})
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
        srv.connect_http(f"http://localhost:{port}", service_config=_retry_service_config())
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
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
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
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
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert seen["user_agent"] == "custom-agent/9.9"
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_retries_using_canonical_error_code_before_http_fallback():
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
                    400,
                    {
                        "code": "unavailable",
                        "message": "temporary outage",
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
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://localhost:{port}", service_config=_retry_service_config())
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
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
                    "code": "unavailable",
                    "message": "temporary outage",
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
                await srv._cli(["greet.v1.GreetService", "GreetGroup", "-r", '{"people":[{"name":"Alice"}]}'])
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
                self._write_json(401, {"code": "unauthenticated", "message": "missing signature"})
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

        srv.connect_http(f"http://localhost:{port}", auth=provider)
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
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

    srv.connect_http("http://localhost:1", auth=provider)
    try:
        with pytest.raises(InvariantError, match="missing signing key") as exc:
            await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
        assert exc.value.code == grpc.StatusCode.UNAUTHENTICATED
    finally:
        await srv.stop()


async def test_connect_http_retry_policy_uses_grpc_codes_full_jitter_and_fresh_auth(monkeypatch):
    import invariant.http_client as http_client

    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_GET(self):
            type(self).attempts += 1
            if type(self).attempts <= 2:
                self._write_json(502, {"message": "bad gateway"})
                return
            self._write_json(200, {"message": "Hello, World"})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_args):
            pass

    highs: list[float] = []
    sleeps: list[float] = []

    def jitter(low, high):
        assert low == 0
        highs.append(high)
        return high / 2

    async def fake_sleep(delay):
        sleeps.append(delay)

    monkeypatch.setattr(http_client, "_retry_jitter", jitter)
    monkeypatch.setattr(http_client, "_retry_sleep", fake_sleep)

    auth_calls = 0

    def auth(_req):
        nonlocal auth_calls
        auth_calls += 1
        return {"X-Attempt": str(auth_calls)}

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(
            f"http://localhost:{port}",
            auth=auth,
            service_config={
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService", "method": "Greet"}],
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "0.2s",
                            "max_backoff": "0.3s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                    }
                ]
            },
        )
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert Handler.attempts == 3
            assert auth_calls == 3
            assert highs == [0.2, 0.3]
            assert sleeps == [0.1, 0.15]
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


@pytest.mark.parametrize(
    ("service_config", "match"),
    [
        (
            {
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService"}],
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "0.1s",
                            "max_backoff": "1s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILIBLE"],
                        },
                    }
                ]
            },
            "unknown gRPC status code",
        ),
        (
            {
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService"}],
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "0.1s",
                            "max_backoff": "1s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": [99],
                        },
                    }
                ]
            },
            "unknown gRPC status code number",
        ),
        (
            {
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService"}],
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "not-a-duration",
                            "max_backoff": "1s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                    }
                ]
            },
            "duration string ending in 's'",
        ),
        ({"method_config": [{"name": [{"method": "Greet"}]}]}, "method requires"),
        (
            {
                "method_config": [
                    {
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "0.1s",
                            "max_backoff": "1s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                    }
                ]
            },
            "name is required",
        ),
        (
            {
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService"}],
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "0.1s",
                            "max_backoff": "1s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                        "retry_unsafe_methods": "false",
                    }
                ]
            },
            "retry_unsafe_methods.*boolean",
        ),
        (
            {"method_config": [{"name": [{"service": "greet.v1.GreetService"}], "retry_unsafe_methods": "yes"}]},
            "retry_unsafe_methods.*boolean",
        ),
        (
            {
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService"}],
                        "retry_policy": {
                            "max_attempts": 3.9,
                            "initial_backoff": "0.1s",
                            "max_backoff": "1s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                    }
                ]
            },
            "max_attempts.*integer",
        ),
        ({"method_config": [{"name": [{}, {}]}]}, "duplicates"),
        (
            {
                "method_config": [
                    {"name": [{"service": "greet.v1.GreetService", "method": "Greet"}]},
                    {"name": [{"service": "greet.v1.GreetService", "method": "Greet"}]},
                ]
            },
            "duplicates",
        ),
    ],
)
def test_connect_http_rejects_invalid_service_config(service_config, match):
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(ValueError, match=match):
        srv.connect_http("http://localhost:1", service_config=service_config)


def test_connect_http_rejects_wrong_typed_auth():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(TypeError, match="auth must be None, HTTPAuth, or a callable header provider"):
        srv.connect_http("http://localhost:1", auth={"X-Api-Key": "k"})


async def test_connect_http_service_config_empty_name_is_default_and_loses_to_specific_configs():
    conn = HTTPConnection(
        base_url="http://localhost:1",
        service_config={
            "method_config": [
                {
                    "name": [{}],
                    "retry_policy": {
                        "max_attempts": 2,
                        "initial_backoff": "0s",
                        "max_backoff": "0s",
                        "backoff_multiplier": 1.0,
                        "retryable_status_codes": ["INTERNAL"],
                    },
                },
                {
                    "name": [{"service": "svc.A"}],
                    "retry_policy": {
                        "max_attempts": 3,
                        "initial_backoff": "0s",
                        "max_backoff": "0s",
                        "backoff_multiplier": 1.0,
                        "retryable_status_codes": ["UNAVAILABLE"],
                    },
                },
                {
                    "name": [{"service": "svc.A", "method": "Foo"}],
                    "retry_policy": {
                        "max_attempts": 4,
                        "initial_backoff": "0s",
                        "max_backoff": "0s",
                        "backoff_multiplier": 1.0,
                        "retryable_status_codes": ["RESOURCE_EXHAUSTED"],
                    },
                },
            ]
        },
    )
    try:
        default = conn.retry_policy_for("/other.Svc/Call")
        service = conn.retry_policy_for("/svc.A/Bar")
        method = conn.retry_policy_for("/svc.A/Foo")
        assert default is not None
        assert service is not None
        assert method is not None
        assert default.max_attempts == 2
        assert service.max_attempts == 3
        assert method.max_attempts == 4
        assert default.retryable_status_codes == frozenset({grpc.StatusCode.INTERNAL})
        assert service.retryable_status_codes == frozenset({grpc.StatusCode.UNAVAILABLE})
        assert method.retryable_status_codes == frozenset({grpc.StatusCode.RESOURCE_EXHAUSTED})
    finally:
        await conn.aclose()


async def test_connect_http_retry_policy_caps_max_attempts_at_five():
    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_GET(self):
            type(self).attempts += 1
            self._write_json(503, {"message": "still down"}, extra_headers={"Retry-After": "0"})

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

        def log_message(self, *_args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(
            f"http://localhost:{port}",
            service_config=_retry_service_config(max_attempts=99),
        )
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert exc.value.code == grpc.StatusCode.UNAVAILABLE
            assert Handler.attempts == 5
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


@pytest.mark.parametrize(("codes", "status"), [([14], 503), (["UNKNOWN"], 520)])
async def test_connect_http_retry_policy_accepts_enum_numbers_and_unknown(codes, status):
    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_GET(self):
            type(self).attempts += 1
            if type(self).attempts == 1:
                self._write_json(status, {"message": "try again"})
                return
            self._write_json(200, {"message": "Hello, World"})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
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
        srv.connect_http(
            f"http://localhost:{port}",
            service_config=_retry_service_config(max_attempts=2, codes=codes),
        )
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert Handler.attempts == 2
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_retry_after_honors_pushback_and_resets_backoff(monkeypatch):
    import invariant.http_client as http_client

    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_GET(self):
            type(self).attempts += 1
            if type(self).attempts == 1:
                self._write_json(503, {"message": "transient"})
                return
            if type(self).attempts == 2:
                self._write_json(503, {"message": "pushback"}, extra_headers={"Retry-After": "1"})
                return
            if type(self).attempts == 3:
                self._write_json(503, {"message": "transient"})
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

        def log_message(self, *_args):
            pass

    highs: list[float] = []
    sleeps: list[float] = []

    def jitter(low, high):
        assert low == 0
        highs.append(high)
        return high / 2

    async def fake_sleep(delay):
        sleeps.append(delay)

    monkeypatch.setattr(http_client, "_retry_jitter", jitter)
    monkeypatch.setattr(http_client, "_retry_sleep", fake_sleep)

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(
            f"http://localhost:{port}",
            service_config={
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService", "method": "Greet"}],
                        "retry_policy": {
                            "max_attempts": 4,
                            "initial_backoff": "0.2s",
                            "max_backoff": "3s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                    }
                ]
            },
        )
        try:
            result = await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert result["message"] == "Hello, World"
            assert sleeps == [0.1, 1.0, 0.1]
            assert highs == [0.2, 0.2]
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_retry_after_above_max_backoff_aborts_with_retry_info(monkeypatch):
    import invariant.http_client as http_client

    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_GET(self):
            type(self).attempts += 1
            self._write_json(503, {"message": "pushback"}, extra_headers={"Retry-After": "30"})

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

        def log_message(self, *_args):
            pass

    sleeps: list[float] = []

    async def fake_sleep(delay):
        sleeps.append(delay)

    monkeypatch.setattr(http_client, "_retry_sleep", fake_sleep)

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(
            f"http://localhost:{port}",
            service_config={
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService", "method": "Greet"}],
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "0.2s",
                            "max_backoff": "1s",
                            "backoff_multiplier": 2.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                    }
                ]
            },
        )
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert exc.value.code == grpc.StatusCode.UNAVAILABLE
            assert Handler.attempts == 1
            assert sleeps == []
            retry_infos = [
                detail
                for detail in exc.value.to_payload()["details"]
                if detail["@type"] == "type.googleapis.com/google.rpc.RetryInfo"
            ]
            assert retry_infos == [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "30s"}]
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_retry_unsafe_methods_opt_in():
    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_POST(self):
            type(self).attempts += 1
            if type(self).attempts == 1:
                self._write_json(503, {"message": "try again"})
                return
            self._write_json(200, {"messages": ["Hello, Alice"], "count": 1})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
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
        srv.connect_http(
            f"http://localhost:{port}",
            service_config=_retry_service_config(method="GreetGroup", retry_unsafe_methods=True),
        )
        try:
            result = await srv._cli(["greet.v1.GreetService", "GreetGroup", "-r", '{"people":[{"name":"Alice"}]}'])
            assert result["count"] == 1
            assert Handler.attempts == 2
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_method_config_override_can_disable_service_retry():
    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_POST(self):
            type(self).attempts += 1
            self._write_json(503, {"message": "still down"})

        def _write_json(self, status: int, payload: dict):
            raw = json.dumps(payload).encode()
            self.send_response(status)
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
        srv.connect_http(
            f"http://localhost:{port}",
            service_config={
                "method_config": [
                    {
                        "name": [{"service": "greet.v1.GreetService"}],
                        "retry_policy": {
                            "max_attempts": 3,
                            "initial_backoff": "0s",
                            "max_backoff": "0s",
                            "backoff_multiplier": 1.0,
                            "retryable_status_codes": ["UNAVAILABLE"],
                        },
                        "retry_unsafe_methods": True,
                    },
                    {"name": [{"service": "greet.v1.GreetService", "method": "GreetGroup"}]},
                ]
            },
        )
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["greet.v1.GreetService", "GreetGroup", "-r", '{"people":[{"name":"Alice"}]}'])
            assert exc.value.code == grpc.StatusCode.UNAVAILABLE
            assert Handler.attempts == 1
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_error_details_are_standard_google_rpc_types():
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            raw = json.dumps(
                {
                    "code": "resource_exhausted",
                    "message": "rate limit exceeded for account 123",
                }
            ).encode()
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", "2")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = _connect_http_server(f"http://localhost:{port}")
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            payload = exc.value.to_payload()
            detail_types = {d["@type"] for d in payload["details"]}
            assert "type.googleapis.com/google.rpc.RetryInfo" in detail_types
            assert "type.googleapis.com/google.rpc.ErrorInfo" in detail_types
            assert "type.googleapis.com/google.rpc.QuotaFailure" in detail_types
            error_info = next(d for d in payload["details"] if d["@type"].endswith("google.rpc.ErrorInfo"))
            assert error_info["reason"] == "HTTP_STATUS_429"
            assert error_info["metadata"]["http_status"] == "429"
            assert "rate limit exceeded" in error_info["metadata"]["body_snippet"]
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_remote_retry_info_wins_over_retry_after_header():
    from google.protobuf import duration_pb2
    from google.rpc import error_details_pb2

    retry_message = error_details_pb2.RetryInfo(retry_delay=duration_pb2.Duration(seconds=7))
    remote_retry = {
        "type": "google.rpc.RetryInfo",
        "value": base64.b64encode(retry_message.SerializeToString()).decode().rstrip("="),
    }
    expanded_retry = {
        "@type": "type.googleapis.com/google.rpc.RetryInfo",
        "retryDelay": "7s",
    }

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            raw = json.dumps(
                {
                    "code": "unavailable",
                    "message": "retry later",
                    "details": [remote_retry],
                }
            ).encode()
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", "1")
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
        srv.connect_http(f"http://localhost:{port}")
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            retry_payloads = [
                detail
                for detail in exc.value.to_payload()["details"]
                if detail["@type"] == "type.googleapis.com/google.rpc.RetryInfo"
            ]
            assert retry_payloads == [expanded_retry]

            retry_trailers = []
            for detail in exc.value.to_status_proto().details:
                if detail.Is(error_details_pb2.RetryInfo.DESCRIPTOR):
                    retry_info = error_details_pb2.RetryInfo()
                    detail.Unpack(retry_info)
                    retry_trailers.append(retry_info.retry_delay.seconds)
            assert retry_trailers == [7]
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_observer_captures_error_responses():
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            raw = json.dumps({"message": "bad upstream"}).encode()
            self.send_response(400)
            self.send_header("Content-Type", "application/json")
            self.send_header("X-Trace-Id", "abc")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_args):
            pass

    seen = {}

    def observer(resp):
        seen["status"] = resp.status_code
        seen["success"] = resp.success
        seen["body"] = resp.body
        seen["duration_ms"] = resp.duration_ms
        seen["trace"] = resp.headers.get("x-trace-id")

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://localhost:{port}", observer=observer)
        try:
            with pytest.raises(InvariantError):
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert seen["status"] == 400
            assert seen["success"] is False
            assert json.loads(seen["body"]) == {"message": "bad upstream"}
            assert seen["duration_ms"] >= 0
            assert seen["trace"] == "abc"
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_preserves_unparseable_details_in_payloads_but_not_grpc_trailer():
    import httpx
    from google.rpc import status_pb2

    from invariant.projections.mcp import mcp_dispatch

    details = [
        {
            "@type": "type.googleapis.com/google.rpc.RetryInfo",
            "retryDelay": "1s",
        },
        {
            "@type": "type.googleapis.com/example.CustomDetail",
            "hint": "custom detail",
        },
        {"hint": "try later"},
    ]

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            raw = json.dumps(
                {
                    "code": "unavailable",
                    "message": "upstream unavailable",
                    "details": details,
                }
            ).encode()
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    backend_port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    try:
        srv.connect_http(f"http://localhost:{backend_port}")

        with pytest.raises(InvariantError) as exc:
            await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
        assert exc.value.to_payload()["details"][:3] == details

        mcp = await mcp_dispatch(
            srv,
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {"name": "greet.v1.GreetService.Greet", "arguments": {"name": "World"}},
            },
        )
        assert mcp["result"]["error"]["details"][:3] == details

        http_port = await srv._start_http(port=0)
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{http_port}") as client:
            response = await client.post("/greet.v1.GreetService/Greet", json={"name": "World"})
        assert response.status_code == 503
        connect_details = response.json()["details"]
        assert all(set(detail) == {"type", "value"} for detail in connect_details)
        connect_types = {detail["type"] for detail in connect_details}
        assert "google.rpc.RetryInfo" in connect_types
        assert "google.rpc.ErrorInfo" in connect_types
        assert "example.CustomDetail" not in connect_types

        grpc_port = await srv._start_grpc(port=0)
        async with grpc.aio.insecure_channel(f"localhost:{grpc_port}") as channel:
            stub = channel.unary_unary(
                "/greet.v1.GreetService/Greet",
                request_serializer=lambda msg: msg.SerializeToString(),
                response_deserializer=greet_pb2.GreetResponse.FromString,
            )
            with pytest.raises(grpc.aio.AioRpcError) as grpc_exc:
                await stub(greet_pb2.GreetRequest(name="World"))
        status = status_pb2.Status()
        status.ParseFromString(grpc_exc.value.trailing_metadata().get("grpc-status-details-bin"))
        detail_types = {detail.type_url for detail in status.details}
        assert "type.googleapis.com/google.rpc.RetryInfo" in detail_types
        assert "type.googleapis.com/example.CustomDetail" not in detail_types
    finally:
        await srv.stop()
        httpd.shutdown()


async def test_connect_http_httpbody_descriptor_redirect_limit_headers_and_observer():
    payload = b"symbol,price\nINV,42\n"
    huge = b"x" * 32
    seen: list = []

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/raw/World":
                self.send_response(302)
                self.send_header("Location", "/download/world.csv")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            if self.path == "/download/world.csv":
                self.send_response(200)
                self.send_header("Content-Type", "text/csv")
                self.send_header("X-Archive-Id", "raw-1")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
                return
            if self.path == "/raw/Huge":
                self.send_response(200)
                self.send_header("Content-Type", "application/octet-stream")
                self.send_header("Content-Length", str(len(huge)))
                self.end_headers()
                self.wfile.write(huge)
                return
            self.send_response(404)
            self.end_headers()

        def log_message(self, *_args):
            pass

    def observer(resp):
        seen.append(resp)

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_bytes(_descriptor_with_raw_httpbody())
        srv.connect_http(
            f"http://localhost:{port}",
            options=ChannelOptions(max_receive_message_size=24),
            observer=observer,
        )
        try:
            resp = await srv.invoke("greet.v1.GreetService.RawBody", greet_pb2.GreetRequest(name="World"))
            assert resp.data == payload
            assert resp.content_type == "text/csv"
            assert seen[-1].success is True
            assert seen[-1].headers["x-archive-id"] == "raw-1"

            with pytest.raises(InvariantError) as exc:
                await srv.invoke("greet.v1.GreetService.RawBody", greet_pb2.GreetRequest(name="Huge"))
            assert exc.value.code == grpc.StatusCode.RESOURCE_EXHAUSTED
            assert seen[-1].success is False
            assert len(seen[-1].body) == 24
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_uses_one_shared_client_per_connection(monkeypatch):
    import invariant.http_client as http_client

    clients = []

    class FakeAsyncClient:
        def __init__(self, **_kwargs):
            self.is_closed = False
            clients.append(self)

        async def aclose(self):
            self.is_closed = True

    monkeypatch.setattr(http_client.httpx, "AsyncClient", FakeAsyncClient)
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http("http://localhost:1")
    try:
        assert len(clients) == 1
    finally:
        await srv.stop()


def test_connect_http_staging_failure_does_not_construct_client(monkeypatch):
    import invariant.http_client as http_client

    clients_constructed: list[object] = []

    def async_client(*args, **kwargs):
        del args, kwargs
        client = object()
        clients_constructed.append(client)
        return client

    def reject_handler(**kwargs):
        del kwargs
        raise RuntimeError("cannot stage HTTP handler")

    monkeypatch.setattr(http_client.httpx, "AsyncClient", async_client)
    monkeypatch.setattr(http_client, "HTTPDynamicHandler", reject_handler)

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(RuntimeError, match="cannot stage HTTP handler"):
        server.connect_http("http://localhost:1")

    assert clients_constructed == []
    assert server.tools == {}
    assert server._http_connections == []


async def test_connect_http_channel_options_read_timeout():
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            import time as _time

            _time.sleep(0.2)
            raw = json.dumps({"message": "late"}).encode()
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
        srv.connect_http(
            f"http://localhost:{port}",
            options=ChannelOptions(connect_timeout=1.0, read_timeout=0.05, write_timeout=1.0, pool_timeout=1.0),
        )
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert exc.value.code == grpc.StatusCode.DEADLINE_EXCEEDED
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_channel_options_proxy_is_used():
    class Handler(BaseHTTPRequestHandler):
        attempts = 0

        def do_GET(self):
            type(self).attempts += 1
            self.send_response(200)
            self.end_headers()

        def log_message(self, *_args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(
            f"http://localhost:{port}",
            options=ChannelOptions(connect_timeout=0.05, read_timeout=0.05, proxy="http://127.0.0.1:1"),
        )
        try:
            with pytest.raises(InvariantError) as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"World"}'])
            assert exc.value.code == grpc.StatusCode.UNAVAILABLE
            assert Handler.attempts == 0
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_google_rpc_details_propagate_through_grpc_projection():
    from google.rpc import status_pb2

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            raw = json.dumps({"code": "resource_exhausted", "message": "quota exhausted"}).encode()
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", "1")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_args):
            pass

    httpd = ThreadingHTTPServer(("localhost", 0), Handler)
    backend_port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    try:
        srv.connect_http(f"http://localhost:{backend_port}")
        grpc_port = await srv._start_grpc(port=0)
        try:
            async with grpc.aio.insecure_channel(f"localhost:{grpc_port}") as channel:
                stub = channel.unary_unary(
                    "/greet.v1.GreetService/Greet",
                    request_serializer=lambda msg: msg.SerializeToString(),
                    response_deserializer=greet_pb2.GreetResponse.FromString,
                )
                with pytest.raises(grpc.aio.AioRpcError) as exc:
                    await stub(greet_pb2.GreetRequest(name="World"))
            metadata = exc.value.trailing_metadata()
            status = status_pb2.Status()
            status.ParseFromString(metadata.get("grpc-status-details-bin"))
            assert status.code == 8
            detail_types = {detail.type_url for detail in status.details}
            assert "type.googleapis.com/google.rpc.RetryInfo" in detail_types
            assert "type.googleapis.com/google.rpc.ErrorInfo" in detail_types
            assert "type.googleapis.com/google.rpc.QuotaFailure" in detail_types
        finally:
            await srv._stop_grpc()
    finally:
        await srv.stop()
        httpd.shutdown()
