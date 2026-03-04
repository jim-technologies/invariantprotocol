"""HTTP projection — supports RPC routes and google.api.http transcoding routes."""

from __future__ import annotations

import json
import threading
import urllib.parse
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import TYPE_CHECKING, Any

from google.api import annotations_pb2
from google.protobuf import descriptor_pool, json_format, message_factory

from invariant.errors import (
    as_invariant_error,
    http_status_for,
    invalid_argument,
    invalid_argument_from_json_error,
    not_found,
)

if TYPE_CHECKING:
    from invariant.server import Server, Tool


@dataclass
class _PathSegment:
    literal: str = ""
    field: str = ""
    multi: bool = False


@dataclass
class _PathTemplate:
    segments: list[_PathSegment]

    @classmethod
    def parse(cls, pattern: str) -> _PathTemplate:
        if not pattern.startswith("/"):
            raise ValueError("path must start with '/'")

        trimmed = pattern.strip("/")
        if not trimmed:
            return cls(segments=[])

        raw_segments = trimmed.split("/")
        segments: list[_PathSegment] = []
        for idx, raw in enumerate(raw_segments):
            if raw.startswith("{") and raw.endswith("}"):
                inner = raw[1:-1]
                field = inner
                wildcard = "*"
                if "=" in inner:
                    field, wildcard = inner.split("=", 1)
                if not field:
                    raise ValueError("empty field in variable segment")
                if wildcard in ("", "*"):
                    segments.append(_PathSegment(field=field))
                    continue
                if wildcard == "**":
                    if idx != len(raw_segments) - 1:
                        raise ValueError("** wildcard is only supported in the final segment")
                    segments.append(_PathSegment(field=field, multi=True))
                    continue
                raise ValueError(f"unsupported wildcard pattern {wildcard!r}")

            segments.append(_PathSegment(literal=raw))
        return cls(segments=segments)

    def match(self, path: str) -> dict[str, str] | None:
        trimmed = path.strip("/")
        parts = [] if not trimmed else trimmed.split("/")

        out: dict[str, str] = {}
        idx = 0
        for seg in self.segments:
            if seg.multi:
                if idx > len(parts):
                    return None
                out[seg.field] = urllib.parse.unquote("/".join(parts[idx:]))
                idx = len(parts)
                continue

            if idx >= len(parts):
                return None

            part = parts[idx]
            if seg.field:
                out[seg.field] = urllib.parse.unquote(part)
            elif part != seg.literal:
                return None
            idx += 1

        if idx != len(parts):
            return None
        return out


@dataclass
class _HTTPBinding:
    method: str
    pattern: str
    body: str
    tool: Tool
    template: _PathTemplate

    @classmethod
    def new(cls, method: str, pattern: str, body: str, tool: Tool) -> _HTTPBinding:
        return cls(
            method=method.upper(),
            pattern=pattern,
            body=body,
            tool=tool,
            template=_PathTemplate.parse(pattern),
        )

    def request_args(
        self,
        query: dict[str, list[str]],
        path_params: dict[str, str],
        body_bytes: bytes,
    ) -> dict[str, Any]:
        args: dict[str, Any] = {}
        self._apply_body(args, body_bytes)

        for field, raw in path_params.items():
            value = self._coerce_scalar(field, raw, source="path")
            _set_nested(args, field, value)

        for field, values in query.items():
            value = self._coerce_query(field, values)
            _set_nested(args, field, value)

        return args

    def _apply_body(self, args: dict[str, Any], body_bytes: bytes) -> None:
        trimmed = body_bytes.strip()

        if self.body == "":
            if trimmed:
                raise invalid_argument("request body is not allowed for this route")
            return

        if not trimmed:
            return

        try:
            decoded = json.loads(trimmed)
        except Exception as e:
            raise invalid_argument(f"invalid JSON body: {e}") from None

        if self.body == "*":
            if not isinstance(decoded, dict):
                raise invalid_argument('request body must be a JSON object for body:"*"')
            args.update(decoded)
            return

        _set_nested(args, self.body, decoded)

    def _coerce_query(self, field: str, values: list[str]) -> Any:
        if not values:
            return None

        schema = _schema_for_path(self.tool.input_schema, field)
        if schema and schema.get("type") == "array":
            item_schema = schema.get("items", {})
            out = []
            for raw in values:
                out.append(self._coerce_raw(raw, item_schema, field, source="query"))
            return out

        if len(values) > 1:
            raise invalid_argument(f'multiple query values provided for non-repeated field "{field}"')

        return self._coerce_raw(values[0], schema, field, source="query")

    def _coerce_scalar(self, field: str, raw: str, *, source: str) -> Any:
        schema = _schema_for_path(self.tool.input_schema, field)
        return self._coerce_raw(raw, schema, field, source=source)

    def _coerce_raw(self, raw: str, schema: dict[str, Any] | None, field: str, *, source: str) -> Any:
        if not schema:
            return raw

        schema_type = schema.get("type")
        try:
            if schema_type == "integer":
                return int(raw)
            if schema_type == "number":
                return float(raw)
            if schema_type == "boolean":
                lowered = raw.lower()
                if lowered in ("true", "1"):
                    return True
                if lowered in ("false", "0"):
                    return False
                raise ValueError("expected boolean")
            return raw
        except ValueError as e:
            raise invalid_argument(f'invalid {source} value for "{field}": {e}') from None


def start_http(server: Server, port: int) -> tuple[ThreadingHTTPServer, int]:
    """Start an HTTP server on the given port and return (httpd, actual_port)."""
    handler_class = _make_handler_class(server)
    httpd = ThreadingHTTPServer(("", port), handler_class)
    actual_port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd, actual_port


def _make_handler_class(server: Server):
    pool = descriptor_pool.Default()
    bindings = _build_http_bindings(server)

    class ConnectHandler(BaseHTTPRequestHandler):
        def do_GET(self):
            self._handle("GET")

        def do_POST(self):
            self._handle("POST")

        def do_PUT(self):
            self._handle("PUT")

        def do_PATCH(self):
            self._handle("PATCH")

        def do_DELETE(self):
            self._handle("DELETE")

        def _handle(self, method: str):
            parsed = urllib.parse.urlsplit(self.path)
            binding, path_params, method_mismatch = _find_binding(bindings, method, parsed.path)
            if binding is None:
                if method_mismatch:
                    self.send_response(405)
                    self.end_headers()
                    return
                self._write_error(not_found(f"Not found: {parsed.path}"))
                return

            try:
                body_bytes = self._read_body()
                query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
                args = binding.request_args(query=query, path_params=path_params, body_bytes=body_bytes)

                req_desc = pool.FindMessageTypeByName(binding.tool.input_type)
                req_class = message_factory.GetMessageClass(req_desc)
                request = req_class()
                try:
                    json_format.ParseDict(args, request)
                except Exception as e:
                    raise invalid_argument_from_json_error(e) from None

                response = server._invoke(binding.tool, request, None)
                resp_dict = (
                    json_format.MessageToDict(response, preserving_proto_field_name=True)
                    if response is not None
                    else {}
                )
                self._write_json(200, resp_dict)
            except Exception as e:
                self._write_error(e)

        def _read_body(self) -> bytes:
            try:
                content_length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                raise invalid_argument("invalid Content-Length header") from None
            if content_length <= 0:
                return b""
            return self.rfile.read(content_length)

        def _write_json(self, status: int, payload: dict[str, Any]):
            data = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _write_error(self, e: Exception):
            err = as_invariant_error(e)
            self._write_json(http_status_for(err.code), {"error": err.to_payload()})

        def log_message(self, format, *args):
            pass  # silence request logs during tests

    return ConnectHandler


def _build_http_bindings(server: Server) -> list[_HTTPBinding]:
    bindings: list[_HTTPBinding] = []
    if getattr(server, "_fds", None) is not None:
        bindings.extend(_build_annotated_bindings(server))

    # Canonical RPC route for compatibility.
    for tool in sorted(server.tools.values(), key=lambda t: t.name):
        pattern = f"/{tool.service_full_name}/{tool.method_name}"
        bindings.append(_HTTPBinding.new("POST", pattern, "*", tool))
    return bindings


def _build_annotated_bindings(server: Server) -> list[_HTTPBinding]:
    out: list[_HTTPBinding] = []
    tool_by_method = {
        f"/{tool.service_full_name}/{tool.method_name}": tool
        for tool in server.tools.values()
    }

    for file_proto in server._fds.file:
        pkg = file_proto.package
        for svc in file_proto.service:
            svc_full = f"{pkg}.{svc.name}" if pkg else svc.name
            for method in svc.method:
                key = f"/{svc_full}/{method.name}"
                tool = tool_by_method.get(key)
                if tool is None:
                    continue
                opts = method.options
                if opts is None or not opts.HasExtension(annotations_pb2.http):
                    continue
                rule = opts.Extensions[annotations_pb2.http]
                out.extend(_bindings_from_rule(rule, tool))

    return out


def _bindings_from_rule(rule, tool: Tool) -> list[_HTTPBinding]:
    method, pattern = _method_and_pattern(rule)
    out = [_HTTPBinding.new(method, pattern, rule.body, tool)]
    for nested in rule.additional_bindings:
        out.extend(_bindings_from_rule(nested, tool))
    return out


def _method_and_pattern(rule) -> tuple[str, str]:
    kind = rule.WhichOneof("pattern")
    if kind == "get":
        return "GET", rule.get
    if kind == "post":
        return "POST", rule.post
    if kind == "put":
        return "PUT", rule.put
    if kind == "delete":
        return "DELETE", rule.delete
    if kind == "patch":
        return "PATCH", rule.patch
    if kind == "custom":
        return rule.custom.kind.upper(), rule.custom.path
    raise ValueError("http rule missing pattern")


def _find_binding(
    bindings: list[_HTTPBinding],
    method: str,
    path: str,
) -> tuple[_HTTPBinding | None, dict[str, str] | None, bool]:
    method_mismatch = False
    for binding in bindings:
        params = binding.template.match(path)
        if params is None:
            continue
        if binding.method == method.upper():
            return binding, params, False
        method_mismatch = True
    return None, None, method_mismatch


def _schema_for_path(schema: dict[str, Any], field_path: str) -> dict[str, Any] | None:
    current = schema
    for part in field_path.split("."):
        props = current.get("properties")
        if not isinstance(props, dict):
            return None
        value = props.get(part)
        if not isinstance(value, dict):
            return None
        current = value
    return current


def _set_nested(root: dict[str, Any], field_path: str, value: Any) -> None:
    parts = field_path.split(".")
    current = root
    for idx, part in enumerate(parts):
        is_last = idx == len(parts) - 1
        if is_last:
            current[part] = value
            return

        existing = current.get(part)
        if existing is None:
            child: dict[str, Any] = {}
            current[part] = child
            current = child
            continue
        if not isinstance(existing, dict):
            conflict = ".".join(parts[: idx + 1])
            raise invalid_argument(f'field path conflict at "{conflict}"')
        current = existing
