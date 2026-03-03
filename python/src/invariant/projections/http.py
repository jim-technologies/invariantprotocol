"""HTTP projection — ConnectRPC-compatible unary POST endpoints."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import TYPE_CHECKING

from google.protobuf import descriptor_pool, json_format, message_factory

if TYPE_CHECKING:
    from invariant.server import Server, Tool


def start_http(server: Server, port: int) -> tuple[HTTPServer, int]:
    """Start an HTTP server on the given port and return (httpd, actual_port)."""
    handler_class = _make_handler_class(server)
    httpd = HTTPServer(("", port), handler_class)
    actual_port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd, actual_port


def _make_handler_class(server: Server):
    pool = descriptor_pool.Default()

    # Build lookup: "/greet.v1.GreetService/Greet" -> Tool
    route_map: dict[str, Tool] = {}
    for tool in server.tools.values():
        route = f"/{tool.service_full_name}/{tool.method_name}"
        route_map[route] = tool

    class ConnectHandler(BaseHTTPRequestHandler):
        def do_POST(self):
            tool = route_map.get(self.path)
            if tool is None:
                self.send_error(404, f"Not found: {self.path}")
                return

            try:
                content_length = int(self.headers.get("Content-Length", 0))
                body = self.rfile.read(content_length)
                json_body = json.loads(body) if body else {}

                req_desc = pool.FindMessageTypeByName(tool.input_type)
                req_class = message_factory.GetMessageClass(req_desc)
                request = req_class()
                json_format.ParseDict(json_body, request)

                response = server._invoke(tool, request, None)

                resp_dict = json_format.MessageToDict(response, preserving_proto_field_name=True)
                resp_bytes = json.dumps(resp_dict).encode()

                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(resp_bytes)))
                self.end_headers()
                self.wfile.write(resp_bytes)
            except Exception as e:
                err = json.dumps({"error": str(e)}).encode()
                self.send_response(400)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(err)))
                self.end_headers()
                self.wfile.write(err)

        def log_message(self, format, *args):
            pass  # silence request logs during tests

    return ConnectHandler
