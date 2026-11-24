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


def _descriptor_with_connect_route() -> bytes:
    """Add connectroute.v1.GreetService: the greet messages, no google.api.http rule."""
    with open(DESCRIPTOR_PATH, "rb") as descriptor_file:
        fds = descriptor_pb2.FileDescriptorSet.FromString(descriptor_file.read())
    route = fds.file.add(
        name="connect_route.proto",
        package="connectroute.v1",
        syntax="proto3",
        dependency=["greet.proto"],
    )
    route.service.add(name="GreetService").method.add(
        name="Greet",
        input_type=".greet.v1.GreetRequest",
        output_type=".greet.v1.GreetResponse",
    )
    return fds.SerializeToString()


async def test_connect_http_sends_empty_unary_message_to_strict_connect_server():
    import asyncio

    import httpx
    import uvicorn
    from conftest import GreetServicer, register_greet

    # The upstream is Invariant's own Connect projection, which answers 415 to
    # a unary request that does not name its codec in Content-Type.
    upstream = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(upstream, GreetServicer())
    strict = upstream.asgi_app()
    seen: list[dict] = []

    async def recording(scope, receive, send):
        if scope["type"] != "http":
            await strict(scope, receive, send)
            return
        request = {"content_type": dict(scope["headers"]).get(b"content-type"), "body": b""}
        seen.append(request)

        async def recorded_receive():
            message = await receive()
            request["body"] += message.get("body", b"")
            return message

        path = "/greet.v1.GreetService/Greet"
        await strict({**scope, "path": path, "raw_path": path.encode()}, recorded_receive, send)

    backend = uvicorn.Server(uvicorn.Config(recording, host="127.0.0.1", port=0, log_level="warning"))
    task = asyncio.create_task(backend.serve())
    client = Server.from_bytes(_descriptor_with_connect_route())
    try:
        for _ in range(200):
            if backend.started and backend.servers:
                break
            await asyncio.sleep(0.01)
        base_url = f"http://127.0.0.1:{backend.servers[0].sockets[0].getsockname()[1]}"

        async with httpx.AsyncClient() as bare:
            response = await bare.post(f"{base_url}/greet.v1.GreetService/Greet")
        assert response.status_code == 415

        # The codec owns Content-Type; a provider cannot replace it.
        client.connect_http(base_url, "connectroute.v1.GreetService", auth=lambda _req: {"Content-Type": "text/plain"})
        result = await client.invoke("connectroute.v1.GreetService.Greet", greet_pb2.GreetRequest())
        assert result.message == "Hi "
        assert seen[-1] == {"content_type": b"application/json", "body": b"{}"}
    finally:
        await client.stop()
        backend.should_exit = True
        await task
        await upstream.stop()


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
            with pytest.raises(InvariantError, match="HTTP 400") as exc:
                await srv._cli(["greet.v1.GreetService", "Greet", "-r", '{"name":"bad"}'])
            assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
            assert exc.value.to_payload()["code"] == "invalid_argument"

            with pytest.raises(InvariantError, match="HTTP 499") as canceled:
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


@pytest.mark.parametrize("gateway_only", [False, True])
async def test_connect_http_uses_connect_http_status_fallbacks_for_malformed_errors(gateway_only):
    expected = {
        400: grpc.StatusCode.INTERNAL,
        401: grpc.StatusCode.UNAUTHENTICATED,
        403: grpc.StatusCode.PERMISSION_DENIED,
        404: grpc.StatusCode.UNIMPLEMENTED,
        429: grpc.StatusCode.RESOURCE_EXHAUSTED,
        502: grpc.StatusCode.UNAVAILABLE,
        503: grpc.StatusCode.UNAVAILABLE,
        504: grpc.StatusCode.UNAVAILABLE,
        418: grpc.StatusCode.UNKNOWN,
        409: grpc.StatusCode.UNKNOWN,
        499: grpc.StatusCode.UNKNOWN,
        **dict.fromkeys((500, 501, 507), grpc.StatusCode.UNKNOWN if gateway_only else grpc.StatusCode.UNAVAILABLE),
        **dict.fromkeys(range(520, 525), grpc.StatusCode.UNAVAILABLE),
    }

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            parsed = urllib.parse.urlsplit(self.path)
            status = int(parsed.path.removeprefix("/v1/greet/"))
            body = b""
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
        srv = Server.from_descriptor(DESCRIPTOR_PATH)
        srv.connect_http(f"http://localhost:{port}", options=ChannelOptions(gateway_only_http_fallback=gateway_only))
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
        with pytest.raises(InvariantError, match="HTTP_AUTH_BUILD_FAILED") as exc:
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


@pytest.mark.parametrize(("codes", "status"), [([14], 503), (["UNAVAILABLE"], 520)])
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
            error_info = next(d for d in payload["details"] if d["@type"].endswith("google.rpc.ErrorInfo"))
            assert error_info["reason"] == "HTTP_STATUS_429"
            assert error_info["metadata"]["http_status"] == "429"
            assert error_info["metadata"]["retry_after"] == "2"
            assert set(error_info["metadata"]) == {"host", "http_status", "retry_after", "error_class"}
            assert "rate limit exceeded" not in json.dumps(payload)
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
            assert seen["body"] == b""
            assert seen["duration_ms"] >= 0
            assert seen["trace"] is None
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


async def test_connect_http_only_forwards_allowlisted_retry_details():
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
        assert details[1] not in exc.value.to_payload()["details"]
        assert details[0] in exc.value.to_payload()["details"]

        mcp = await mcp_dispatch(
            srv,
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {"name": "greet.v1.GreetService.Greet", "arguments": {"name": "World"}},
            },
        )
        assert details[1] not in mcp["result"]["error"]["details"]
        assert details[0] in mcp["result"]["error"]["details"]

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


@pytest.mark.parametrize("etag", [b'W/"world,42"', bytes.fromhex("22 e2 82 ac 22"), bytes.fromhex("22 ff 22")])
async def test_connect_http_httpbody_descriptor_redirect_limit_headers_and_observer(etag):
    from invariant.gen.invariant.v1.types_pb2 import HttpEntityTag

    header = etag.decode("latin-1")
    conditional = {}
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
                if self.headers.get("If-None-Match") == header:
                    self.send_response(304)
                    self.send_header("ETag", header)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("ETag", header)
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
            auth=HTTPAuth(header_provider=lambda _: conditional),
        )
        try:
            resp = await srv.invoke("greet.v1.GreetService.RawBody", greet_pb2.GreetRequest(name="World"))
            assert resp.data == payload
            assert resp.content_type == "text/csv"
            assert seen[-1].success is True
            assert seen[-1].headers == {"content-type": "text/csv", "etag": header}
            assert seen[-1].metadata["etag"] == header
            validator = HttpEntityTag()
            assert resp.extensions[0].Unpack(validator)
            assert validator.etag == etag
            assert seen[-1].request.url == f"http://localhost:{port}/raw/World"
            assert seen[-1].url == f"http://localhost:{port}/download/world.csv"

            conditional["If-None-Match"] = validator.etag
            unchanged = await srv.invoke("greet.v1.GreetService.RawBody", greet_pb2.GreetRequest(name="World"))
            assert not unchanged.data
            assert seen[-1].status_code == 304
            assert seen[-1].headers["etag"] == header

            with pytest.raises(InvariantError) as exc:
                await srv.invoke("greet.v1.GreetService.RawBody", greet_pb2.GreetRequest(name="Huge"))
            assert exc.value.code == grpc.StatusCode.RESOURCE_EXHAUSTED
            assert seen[-1].success is False
            assert not seen[-1].body
        finally:
            await srv.stop()
    finally:
        httpd.shutdown()


@pytest.mark.parametrize("admitted", [False, True])
@pytest.mark.parametrize("httpbody", [False, True])
@pytest.mark.parametrize("follow_redirects", [None, False, True])
async def test_connect_http_redirect_policy_controls_cross_origin_requests(httpbody, follow_redirects, admitted):
    payload = b'{"message":"redirected"}'
    target_requests: list[str] = []
    observed: list = []

    class TargetHandler(BaseHTTPRequestHandler):
        def do_GET(self):
            target_requests.append(self.path)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *_args):
            pass

    target = ThreadingHTTPServer(("localhost", 0), TargetHandler)
    target_url = f"http://localhost:{target.server_address[1]}/redirect-target"

    class SourceHandler(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(302)
            self.send_header("Location", target_url)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *_args):
            pass

    source = ThreadingHTTPServer(("localhost", 0), SourceHandler)
    for backend in (source, target):
        threading.Thread(target=backend.serve_forever, daemon=True).start()
    srv = Server.from_bytes(_descriptor_with_raw_httpbody()) if httpbody else Server.from_descriptor(DESCRIPTOR_PATH)
    origin = f"http://localhost:{source.server_address[1]}"
    srv.connect_http(
        origin,
        options=ChannelOptions(
            follow_redirects=follow_redirects,
            allow_cross_origin_redirects=True,
            # The admitted send path walks redirects itself; it must honour the same policy.
            before_http_attempt=(lambda: None) if admitted else None,
        ),
        observer=observed.append,
    )
    try:
        method = "RawBody" if httpbody else "Greet"
        response = await srv.invoke(f"greet.v1.GreetService.{method}", greet_pb2.GreetRequest(name="World"))
        follows = httpbody if follow_redirects is None else follow_redirects
        assert len(observed) == 1
        assert observed[0].request.url.startswith(origin + "/")
        if follows:
            assert target_requests == ["/redirect-target"]
            assert observed[0].status_code == 200
            assert observed[0].url == target_url
            assert observed[0].body == payload
            assert response.data == payload if httpbody else response.message == "redirected"
        else:
            assert target_requests == []
            assert observed[0].status_code == 302
            assert observed[0].url == observed[0].request.url
            assert observed[0].body == b""
            assert response.data == b"" if httpbody else response.message == ""
    finally:
        await srv.stop()
        for backend in (source, target):
            backend.shutdown()
            backend.server_close()


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
        finally:
            await srv._stop_grpc()
    finally:
        await srv.stop()
        httpd.shutdown()


@pytest.mark.parametrize("failure", ["status", "exception", "invalid_url", "pool"])
async def test_http_transport_redacts_credentials_in_payload_observer_and_logs(caplog, failure):
    import logging

    import httpx

    markers = ("password_secret", "path_secret", "query_secret", "body_secret", "header_secret")
    target = f"https://user:{markers[0]}@provider.test/{markers[1]}"
    events = []

    def respond(request):
        assert markers[2] in str(request.url)
        logging.getLogger("hpack.hpack").debug("encoding :path %s", request.url)
        if failure == "invalid_url":
            raise httpx.InvalidURL(f"invalid URL {request.url}")
        if failure == "pool":
            raise httpx.PoolTimeout(f"no connection for {request.url}", request=request)
        if failure == "exception":
            raise httpx.LocalProtocolError(f"{request.url} {markers[3]}", request=request)
        return httpx.Response(503, content=markers[3].encode(), headers={"Retry-After": markers[4]})

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    server.connect_http(
        target,
        auth=HTTPAuth(query_provider=lambda _: {"api_key": markers[2]}),
        observer=events.append,
    )
    connection = server._http_connections[0]
    connection._client = httpx.AsyncClient(transport=httpx.MockTransport(respond))
    try:
        with caplog.at_level(logging.DEBUG), pytest.raises(InvariantError) as caught:
            await server.invoke("greet.v1.GreetService.Greet", greet_pb2.GreetRequest(name="test"))
        serialized = json.dumps(caught.value.to_payload())
        serialized += str(caught.value.to_status_proto()) + caplog.text + repr(events)
        assert all(marker not in serialized for marker in markers)
        detail = caught.value.details[-1]
        assert detail["metadata"]["host"] == "provider.test"
        if failure == "status":
            assert caught.value.code == grpc.StatusCode.UNAVAILABLE
            assert detail["metadata"]["http_status"] == "503"
            assert "retry_after" not in detail["metadata"]
            assert events
            assert not events[0].body
            assert not events[0].headers
        elif failure == "pool":
            assert caught.value.code == grpc.StatusCode.DEADLINE_EXCEEDED
            assert detail["reason"] == "LOCAL_HTTP_FAILURE"
            assert detail["metadata"]["error_class"] == "PoolTimeout"
        else:
            expected_code = grpc.StatusCode.INTERNAL if failure == "invalid_url" else grpc.StatusCode.UNAVAILABLE
            assert caught.value.code == expected_code
            assert detail["reason"] == "LOCAL_HTTP_FAILURE"
            expected = "InvalidURL" if failure == "invalid_url" else "LocalProtocolError"
            assert detail["metadata"]["error_class"] == expected
    finally:
        await server.stop()


async def test_call_deadline_shorter_than_retry_after_sends_once(monkeypatch):
    import asyncio

    import httpx

    from invariant import http_client

    calls = 0
    sleeping = asyncio.Event()

    def respond(request):
        nonlocal calls
        calls += 1
        return httpx.Response(503, headers={"Retry-After": "1"})

    async def sleep(delay):
        assert delay == 1
        sleeping.set()
        await asyncio.sleep(delay)

    monkeypatch.setattr(http_client, "_retry_sleep", sleep)
    server = Server.from_descriptor(DESCRIPTOR_PATH)
    server.connect_http("https://provider.test", service_config=_retry_service_config())
    server._http_connections[0]._client = httpx.AsyncClient(transport=httpx.MockTransport(respond))
    try:
        async with asyncio.timeout(0.2):
            task = asyncio.create_task(
                server.invoke("greet.v1.GreetService.Greet", greet_pb2.GreetRequest(name="test"))
            )
            await sleeping.wait()
        with pytest.raises(TimeoutError):
            await asyncio.wait_for(task, timeout=0.01)
        assert calls == 1
    finally:
        await server.stop()


async def test_transport_does_not_wait_for_pushback_beyond_call_context_deadline(monkeypatch):
    from types import SimpleNamespace

    import httpx

    from invariant import http_client

    def respond(request):
        return httpx.Response(503, headers={"Retry-After": "1"})

    async def unexpected_sleep(_delay):
        pytest.fail("pushback exceeds the call deadline")

    monkeypatch.setattr(http_client, "_retry_sleep", unexpected_sleep)
    server = Server.from_descriptor(DESCRIPTOR_PATH)
    server.connect_http("https://provider.test", service_config=_retry_service_config())
    server._http_connections[0]._client = httpx.AsyncClient(transport=httpx.MockTransport(respond))
    try:
        with pytest.raises(InvariantError) as caught:
            await server.invoke(
                "greet.v1.GreetService.Greet",
                greet_pb2.GreetRequest(name="test"),
                SimpleNamespace(time_remaining=lambda: 0.2),
            )
        assert caught.value.code == grpc.StatusCode.UNAVAILABLE
        assert caught.value.details[-1]["metadata"]["retry_after"] == "1"
    finally:
        await server.stop()


async def test_refused_post_redirect_raises_and_only_sends_once():
    import httpx

    sends = []

    def response(request):
        sends.append(request.method)
        return httpx.Response(307, headers={"Location": "https://other.test/hidden?token=secret"})

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http("https://provider.test", options=ChannelOptions(follow_redirects=True))
    srv._http_connections[0]._client = httpx.AsyncClient(transport=httpx.MockTransport(response))
    try:
        with pytest.raises(InvariantError, match="HTTP 307") as error:
            await srv._cli(["greet.v1.GreetService", "GreetGroup", "-r", '{"people":[]}'])
        assert error.value.details[-1]["metadata"]["host"] == "provider.test"
        assert sends == ["POST"]
    finally:
        await srv.stop()


async def test_outage_uses_redirected_host():
    import httpx

    def response(request):
        if request.url.host == "provider.test":
            return httpx.Response(302, headers={"Location": "https://other.test"})
        return httpx.Response(520)

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http(
        "https://provider.test", options=ChannelOptions(follow_redirects=True, allow_cross_origin_redirects=True)
    )
    srv._http_connections[0]._client = httpx.AsyncClient(transport=httpx.MockTransport(response))
    try:
        with pytest.raises(InvariantError, match="HTTP 520") as error:
            await srv.invoke("greet.v1.GreetService.Greet", greet_pb2.GreetRequest(name="test"))
        assert error.value.details[-1]["metadata"]["host"] == "other.test"
    finally:
        await srv.stop()


async def test_sensitive_http_data_absent_from_observation_chains_and_otel(caplog):
    import logging

    import httpx
    from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    markers = ["secret-user", "secret-path", "secret-query", "secret-header", "secret-cookie"]
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    observed = []
    errors = []
    outcomes = iter(("success", "exception", "auth"))

    def auth(request):
        state = next(outcomes)
        if state == "auth":
            raise ValueError(f"failed: {request!r} {request.url} {markers[3]}")
        return {"X-Private": markers[3], "X-State": state}

    def response(request):
        if request.headers["X-State"] == "exception":
            raise httpx.ReadError(f"failed: {request.url} {markers[-1]}", request=request)
        return httpx.Response(
            200,
            json={"message": "ok"},
            headers={
                "Set-Cookie": f"sid={markers[-1]}",
                "X-Private": markers[3],
                "Record-Total": "042",
                "X-OddsAPI-Seq": "123",
            },
        )

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http(
        f"https://{markers[0]}@provider.test/{markers[1]}",
        auth=HTTPAuth(header_provider=auth, query_provider=lambda _: {"key": markers[2]}),
        observer=observed.append,
    )
    instrumentor = HTTPXClientInstrumentor()
    instrumentor.instrument(tracer_provider=provider)
    srv._http_connections[0]._client = httpx.AsyncClient(transport=httpx.MockTransport(response))
    try:
        with caplog.at_level(logging.DEBUG), provider.get_tracer(__name__).start_as_current_span("outer"):
            request = greet_pb2.GreetRequest(name="test")
            await srv.invoke("greet.v1.GreetService.Greet", request)
            for _ in range(2):
                with pytest.raises(InvariantError) as caught:
                    await srv.invoke("greet.v1.GreetService.Greet", request)
                errors.append(caught.value)
                assert caught.value.__cause__ is None
                assert caught.value.__context__ is None
                logging.getLogger(__name__).info("sanitized", extra={"error_repr": repr(caught.value)})
        assert observed[0].headers == {"content-type": "application/json", "record-total": "42", "x-oddsapi-seq": "123"}
        combined = repr(observed) + repr(vars(observed[0])) + repr(vars(observed[0].request))
        combined += repr([error.to_payload() for error in errors])
        combined += repr([vars(record) for record in caplog.records])
        spans = exporter.get_finished_spans()
        assert [span.name for span in spans] == ["outer"]
        combined += repr([dict(span.attributes or {}) for span in spans])
        assert all(marker not in combined for marker in markers)
    finally:
        instrumentor.uninstrument()
        await srv.stop()
        provider.shutdown()


@pytest.mark.parametrize("admitted", [False, True])
@pytest.mark.parametrize("allow_cross_origin", [False, True])
async def test_cross_origin_redirect_credentials_never_reach_destination(admitted, allow_cross_origin):
    import httpx

    sent = []
    marker = "synthetic-marker"

    def send(request):
        sent.append(request)
        if request.url.host == "provider.test":
            return httpx.Response(302, headers={"Location": f"https://{marker}@other.test/finish?key={marker}"})
        return httpx.Response(200, json={"message": "done"})

    async def source_hook(request):
        request.headers["X-Hook"] = marker

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http(
        "https://provider.test",
        auth=HTTPAuth(
            header_provider=lambda _: {"Authorization": marker, "X-Api-Key": marker, "X-Custom": marker},
            query_provider=lambda _: {"key": marker},
        ),
        options=ChannelOptions(
            follow_redirects=True,
            allow_cross_origin_redirects=allow_cross_origin,
            before_http_attempt=(lambda: None) if admitted else None,
        ),
    )
    srv._http_connections[0]._client = httpx.AsyncClient(
        headers={"X-Default": marker},
        auth=(marker, marker),
        cookies={"initial": marker},
        event_hooks={"request": [source_hook]},
        transport=httpx.MockTransport(send),
    )
    try:
        request = greet_pb2.GreetRequest(name="test")
        if not allow_cross_origin:
            with pytest.raises(InvariantError, match="cross-origin HTTP redirect refused"):
                await srv.invoke("greet.v1.GreetService.Greet", request)
            assert len(sent) == 1
        else:
            assert (await srv.invoke("greet.v1.GreetService.Greet", request)).message == "done"
            assert len(sent) == 2
            assert str(sent[1].url) == "https://other.test/finish"
            assert marker not in repr(dict(sent[1].headers))
            for name in ("authorization", "cookie", "x-api-key", "x-custom", "x-default", "x-hook"):
                assert name not in sent[1].headers
        assert sent[0].headers["X-Api-Key"] == marker
        assert sent[0].headers["Cookie"] == f"initial={marker}"
        assert sent[0].url.params["key"] == marker
    finally:
        await srv.stop()


async def test_httpx_hook_exception_and_ambient_context_are_removed_from_diagnostics(caplog):
    import logging

    import httpx
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    secrets = ("hook-query", "hook-header", "hook-path", "hook-cookie", "hook-user", "caller-secret")
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracer = provider.get_tracer(__name__)

    async def hook(request):
        raise RuntimeError(f"{request.url} {dict(request.headers)}")

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http(
        f"https://{secrets[4]}@provider.test/{secrets[2]}",
        auth=HTTPAuth(query_provider=lambda _: {"key": secrets[0]}, header_provider=lambda _: {"X-Key": secrets[1]}),
    )
    srv._http_connections[0]._client = httpx.AsyncClient(
        cookies={"sid": secrets[3]},
        event_hooks={"request": [hook]},
        transport=httpx.MockTransport(lambda _: httpx.Response(200)),
    )
    try:
        try:
            raise ValueError(secrets[5])
        except ValueError:
            try:
                with tracer.start_as_current_span("caller"):
                    await srv.invoke("greet.v1.GreetService.Greet", greet_pb2.GreetRequest(name="test"))
            except InvariantError as error:
                assert error.code == grpc.StatusCode.INTERNAL
                assert error.__cause__ is None
                assert error.__context__ is None
                logging.getLogger(__name__).exception("safe transport failure")
            else:
                pytest.fail("hook did not fail")
        (span,) = exporter.get_finished_spans()
        assert span.events[0].name == "exception"
        output = caplog.text + repr(dict(span.events[0].attributes or {}))
        assert all(marker not in output for marker in secrets)
    finally:
        await srv.stop()
        provider.shutdown()


def test_invalid_origin_port_reports_no_origin_text():
    with pytest.raises(ValueError, match="valid host and port") as error:
        HTTPConnection(base_url="https://provider.test:synthetic-secret/path")
    assert "synthetic-secret" not in str(error.value)
    assert error.value.__context__ is None


@pytest.mark.parametrize(("delay", "remaining", "expected"), [(0.5, 3.0, [0.5]), (0.5, 0.2, []), (30, 40, [])])
async def test_connect_retry_info_controls_retry_sleep_and_send_count(monkeypatch, delay, remaining, expected):
    from types import SimpleNamespace

    import httpx

    from invariant import http_client

    sleeps = []
    sends = []

    def send(request):
        sends.append(request)
        if len(sends) > 1:
            return httpx.Response(200, json={"message": "done"})
        return httpx.Response(
            503,
            json={
                "code": "unavailable",
                "details": [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": f"{delay}s"}],
            },
        )

    async def sleep(seconds):
        sleeps.append(seconds)

    monkeypatch.setattr(http_client, "_retry_sleep", sleep)
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http("https://provider.test", service_config=_retry_service_config())
    srv._http_connections[0]._client = httpx.AsyncClient(transport=httpx.MockTransport(send))
    context = SimpleNamespace(time_remaining=lambda: remaining)
    request = greet_pb2.GreetRequest(name="test")
    try:
        if expected:
            assert (await srv.invoke("greet.v1.GreetService.Greet", request, context)).message == "done"
        else:
            with pytest.raises(InvariantError, match="HTTP 503"):
                await srv.invoke("greet.v1.GreetService.Greet", request, context)
        assert sleeps == expected
        assert len(sends) == 1 + len(expected)
    finally:
        await srv.stop()


@pytest.mark.parametrize(
    ("status", "code"), [(400, grpc.StatusCode.INVALID_ARGUMENT), (403, grpc.StatusCode.RESOURCE_EXHAUSTED)]
)
async def test_connect_envelope_survives_content_type_mismatch(status, code):
    import httpx

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http("https://provider.test")
    envelope = {"code": code.name.lower(), "message": "synthetic-secret"}
    srv._http_connections[0]._client = httpx.AsyncClient(
        transport=httpx.MockTransport(
            lambda _: httpx.Response(status, content=json.dumps(envelope), headers={"Content-Type": "text/plain"})
        )
    )
    try:
        with pytest.raises(InvariantError) as error:
            await srv.invoke("greet.v1.GreetService.Greet", greet_pb2.GreetRequest(name="test"))
        assert error.value.code == code
        kinds = [d["@type"] for d in error.value.details]
        assert ("type.googleapis.com/google.rpc.QuotaFailure" in kinds) == (status == 403)
        assert "synthetic-secret" not in json.dumps(error.value.to_payload())
    finally:
        await srv.stop()


@pytest.mark.parametrize("admitted", [False, True])
async def test_signed_cross_origin_download_and_destination_cookies_are_isolated(monkeypatch, admitted):
    import httpx

    from invariant import http_client

    requests = []
    signed_query = b"signature=cdn%2Fsig%2Bvalue&part=1"

    async def sleep(_seconds):
        return None

    monkeypatch.setattr(http_client, "_retry_sleep", sleep)

    def send(request):
        requests.append(request)
        if request.url.host == "origin.example.test":
            return httpx.Response(
                302,
                headers={
                    "Location": "https://cdn.example.test/download?" + signed_query.decode() + "&signature=source-key"
                },
            )
        assert request.url.query == signed_query
        assert "cookie" not in request.headers
        if len(requests) == 2:
            return httpx.Response(503, headers={"Set-Cookie": "foreign=cdn-cookie; Domain=.example.test; Path=/"})
        return httpx.Response(200, json={"message": "downloaded"})

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.connect_http(
        "https://origin.example.test",
        auth=HTTPAuth(query_provider=lambda _: {"signature": "source-key"}),
        service_config=_retry_service_config(),
        options=ChannelOptions(
            follow_redirects=True,
            allow_cross_origin_redirects=True,
            before_http_attempt=(lambda: None) if admitted else None,
        ),
    )
    srv._http_connections[0]._client = httpx.AsyncClient(
        cookies={"local": "origin-cookie"}, transport=httpx.MockTransport(send)
    )
    try:
        result = await srv.invoke("greet.v1.GreetService.Greet", greet_pb2.GreetRequest(name="test"))
        assert result.message == "downloaded"
        assert [request.url.host for request in requests] == [
            "origin.example.test",
            "cdn.example.test",
            "origin.example.test",
            "cdn.example.test",
        ]
        assert requests[2].headers["Cookie"] == "local=origin-cookie"
        assert requests[2].url.params["signature"] == "source-key"
    finally:
        await srv.stop()


async def test_http_error_classifier_marks_a_bounded_refusal_without_exposing_bytes():
    raw = b'{"state":"closed","private":"secret-value"}'

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(503)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_args):
            pass

    calls = []
    observed = []

    def classify(method, status, body):
        calls.append((method, status))
        return json.loads(body)["state"] == "closed"

    with ThreadingHTTPServer(("localhost", 0), Handler) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        srv = Server.from_bytes(_descriptor_with_raw_httpbody())
        srv.connect_http(
            f"http://localhost:{server.server_port}",
            observer=observed.append,
            options=ChannelOptions(classify_response_refusal=classify),
            service_config=_retry_service_config(),
        )
        try:
            with pytest.raises(InvariantError) as caught:
                await srv.invoke("greet.v1.GreetService.RawBody", greet_pb2.GreetRequest(name="World"))
            assert calls == [("/greet.v1.GreetService/RawBody", 503)]
            assert caught.value.details[0]["reason"] == "HTTP_PROVIDER_REFUSAL"
            assert "secret-value" not in repr(caught.value.details)
            assert len(observed) == 1
            assert not observed[0].body
        finally:
            await srv.stop()
            server.shutdown()
            thread.join()


@pytest.mark.parametrize("binary", [False, True])
def test_connect_http_only_exposes_selected_error_info(binary):
    import httpx
    from google.rpc import error_details_pb2

    from invariant.http_client import _http_error

    info = error_details_pb2.ErrorInfo(domain="demo", reason="ALREADY_COMMITTED", metadata={"private": "never-forward"})
    selected = (
        {"type": "google.rpc.ErrorInfo", "value": base64.b64encode(info.SerializeToString()).decode()}
        if binary
        else {
            "@type": "type.googleapis.com/google.rpc.ErrorInfo",
            "domain": info.domain,
            "reason": info.reason,
            "metadata": {"private": "never-forward"},
        }
    )
    raw = json.dumps({"code": "already_exists", "message": "never-forward", "details": [selected]}).encode()
    error, _ = _http_error(
        409,
        raw,
        httpx.Headers(),
        host="example.test",
        connect_error_reasons=frozenset({("demo", "ALREADY_COMMITTED")}),
    )
    assert error.code is grpc.StatusCode.ALREADY_EXISTS
    assert error.details[0]["domain"] == "demo"
    assert error.details[0]["reason"] == "ALREADY_COMMITTED"
    assert "never-forward" not in str(error) + repr(error.details)
    unconfigured, _ = _http_error(409, raw, httpx.Headers(), host="example.test")
    assert all(detail.get("domain") != "demo" for detail in unconfigured.details)
