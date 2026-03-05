"""Descriptor-driven HTTP client for proxying RPC tools to REST endpoints."""

from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from email.utils import parsedate_to_datetime
from typing import Any

import grpc
from google.api import annotations_pb2
from google.protobuf import descriptor_pool, json_format, message_factory

from invariant.errors import InvariantError, invalid_argument

_DEFAULT_HTTP_MAX_RETRIES = 2
_BASE_HTTP_RETRY_DELAY_SECONDS = 0.1
_MAX_HTTP_RETRY_DELAY_SECONDS = 2.0
_RETRYABLE_HTTP_STATUSES = {429, 500, 502, 503, 504}


@dataclass
class HTTPClientBinding:
    method: str
    pattern: str
    body: str
    template: _PathTemplate

    @classmethod
    def new(cls, method: str, pattern: str, body: str) -> HTTPClientBinding:
        return cls(
            method=method.upper(),
            pattern=pattern,
            body=body,
            template=_PathTemplate.parse(pattern),
        )

    def build(self, args: dict[str, Any], base_url: str) -> tuple[bytes | None, str]:
        working = _clone_map(args)

        path, consumed = self._expand_path(working)
        for field in consumed:
            _delete_nested(working, field)

        body_bytes, consumed_body = self._build_body(working)
        if consumed_body and consumed_body != "*":
            _delete_nested(working, consumed_body)

        query = ""
        if self.body != "*":
            params: list[tuple[str, str]] = []
            _encode_query_fields("", working, params)
            if params:
                query = urllib.parse.urlencode(params, doseq=True)

        base = base_url.rstrip("/")
        full_url = f"{base}{path}"
        if query:
            full_url += f"?{query}"
        return body_bytes, full_url

    def _expand_path(self, args: dict[str, Any]) -> tuple[str, list[str]]:
        if not self.template.segments:
            return "/", []

        parts: list[str] = []
        consumed: list[str] = []
        for seg in self.template.segments:
            if seg.field:
                value, ok = _get_nested(args, seg.field)
                if not ok:
                    raise invalid_argument(f'missing path field "{seg.field}"')
                parts.append(_encode_path_value(value, multi=seg.multi))
                consumed.append(seg.field)
            else:
                parts.append(seg.literal)

        return "/" + "/".join(parts), consumed

    def _build_body(self, args: dict[str, Any]) -> tuple[bytes | None, str | None]:
        if self.body == "":
            return None, None

        if self.body == "*":
            if not args:
                return None, "*"
            return json.dumps(args).encode(), "*"

        value, ok = _get_nested(args, self.body)
        if not ok:
            return None, self.body
        return json.dumps(value).encode(), self.body


class HTTPDynamicHandler:
    """Callable tool handler that proxies to a remote HTTP endpoint."""

    def __init__(
        self,
        *,
        base_url: str,
        binding: HTTPClientBinding,
        output_type: str,
        timeout: float,
        method_path: str,
        header_provider=None,
    ) -> None:
        self._base_url = _validated_base_url(base_url)
        self._binding = binding
        self._timeout = timeout
        self._max_retries = _DEFAULT_HTTP_MAX_RETRIES
        self._method_path = method_path
        self._header_provider = header_provider
        self._headers = _outbound_http_headers_from_env()
        pool = descriptor_pool.Default()
        resp_desc = pool.FindMessageTypeByName(output_type)
        self._resp_class = message_factory.GetMessageClass(resp_desc)

    def __call__(self, request, _context):
        args = json_format.MessageToDict(request, preserving_proto_field_name=True)
        body_bytes, target = self._binding.build(args, self._base_url)
        attempt = 0

        while True:
            req = urllib.request.Request(  # noqa: S310
                target,
                data=body_bytes,
                method=self._binding.method,
            )
            req.add_header("Accept", "application/json")
            if body_bytes is not None:
                req.add_header("Content-Type", "application/json")
            for name, value in self._headers.items():
                req.add_header(name, value)
            self._add_dynamic_headers(req, target, body_bytes)

            try:
                with urllib.request.urlopen(req, timeout=self._timeout) as resp:  # noqa: S310
                    raw = resp.read()
            except urllib.error.HTTPError as e:
                body = e.read()
                if self._should_retry(attempt, status_code=e.code):
                    delay = _retry_delay_seconds(attempt, e.headers.get("Retry-After") if e.headers else None)
                    _sleep_seconds(delay)
                    attempt += 1
                    continue
                raise _http_error(e.code, body) from None
            except urllib.error.URLError as e:
                if self._should_retry(attempt, status_code=0):
                    _sleep_seconds(_retry_delay_seconds(attempt, None))
                    attempt += 1
                    continue
                raise InvariantError(grpc.StatusCode.UNAVAILABLE, f"HTTP request failed: {e}") from None

            out = self._resp_class()
            if raw.strip():
                try:
                    json_format.Parse(raw.decode(), out, ignore_unknown_fields=True)
                except Exception as e:
                    raise InvariantError(grpc.StatusCode.INTERNAL, f"decode HTTP response JSON: {e}") from None
            return out

    def _should_retry(self, attempt: int, *, status_code: int) -> bool:
        if attempt >= self._max_retries:
            return False
        if not _is_safe_retry_method(self._binding.method):
            return False
        if status_code == 0:
            return True
        return status_code in _RETRYABLE_HTTP_STATUSES

    def _add_dynamic_headers(self, req: urllib.request.Request, target: str, body_bytes: bytes | None) -> None:
        if self._header_provider is None:
            return

        try:
            from invariant.server import OutboundHTTPRequest

            dynamic_headers = self._header_provider(
                OutboundHTTPRequest(
                    method_path=self._method_path,
                    method=self._binding.method,
                    url=target,
                    body=body_bytes or b"",
                )
            )
        except InvariantError:
            raise
        except Exception as e:
            raise InvariantError(
                grpc.StatusCode.UNAUTHENTICATED,
                f"build outbound HTTP headers for {self._method_path}: {e}",
            ) from None

        if not dynamic_headers:
            return
        for name, value in dynamic_headers.items():
            if not name or not value:
                continue
            lowered = name.lower()
            if lowered in ("accept", "content-type"):
                continue
            req.add_header(name, value)


def http_rules_by_method_path(fds) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for file_proto in fds.file:
        pkg = file_proto.package
        for svc in file_proto.service:
            svc_full = f"{pkg}.{svc.name}" if pkg else svc.name
            for method in svc.method:
                opts = method.options
                if opts is None or not opts.HasExtension(annotations_pb2.http):
                    continue
                out[f"/{svc_full}/{method.name}"] = opts.Extensions[annotations_pb2.http]
    return out


def client_binding_for_method(rule, service_full_name: str, method_name: str) -> HTTPClientBinding:
    if rule is None:
        return HTTPClientBinding.new("POST", f"/{service_full_name}/{method_name}", "*")

    method, pattern = _method_and_pattern(rule)
    return HTTPClientBinding.new(method, pattern, rule.body)


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


def _clone_map(data: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in data.items():
        if isinstance(value, dict):
            out[key] = _clone_map(value)
        elif isinstance(value, list):
            out[key] = _clone_list(value)
        else:
            out[key] = value
    return out


def _clone_list(data: list[Any]) -> list[Any]:
    out: list[Any] = []
    for value in data:
        if isinstance(value, dict):
            out.append(_clone_map(value))
        elif isinstance(value, list):
            out.append(_clone_list(value))
        else:
            out.append(value)
    return out


def _get_nested(root: dict[str, Any], path: str) -> tuple[Any, bool]:
    current: Any = root
    for part in path.split("."):
        if not isinstance(current, dict):
            return None, False
        if part not in current:
            return None, False
        current = current[part]
    return current, True


def _delete_nested(root: dict[str, Any], path: str) -> None:
    parts = path.split(".")
    if not parts:
        return
    current = root
    for part in parts[:-1]:
        child = current.get(part)
        if not isinstance(child, dict):
            return
        current = child
    current.pop(parts[-1], None)


def _encode_path_value(value: Any, *, multi: bool) -> str:
    raw = _scalar_to_string(value)
    if not multi:
        return urllib.parse.quote(raw, safe="")
    return "/".join(urllib.parse.quote(chunk, safe="") for chunk in raw.split("/"))


def _encode_query_fields(prefix: str, value: Any, out: list[tuple[str, str]]) -> None:
    if value is None:
        return
    if isinstance(value, dict):
        for key in sorted(value.keys()):
            child = key if not prefix else f"{prefix}.{key}"
            _encode_query_fields(child, value[key], out)
        return
    if isinstance(value, list):
        for item in value:
            out.append((prefix, _scalar_to_string(item)))
        return
    out.append((prefix, _scalar_to_string(value)))


def _scalar_to_string(value: Any) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, str | int | float):
        return str(value)
    raise invalid_argument(f"expected scalar value, got {type(value).__name__}")


def _http_error(status_code: int, body: bytes) -> InvariantError:
    code = _grpc_code_from_http_status(status_code)
    message = f"HTTP {status_code}"
    details = None

    try:
        payload = json.loads(body.decode() if isinstance(body, bytes) else body)
    except Exception:
        payload = {}

    if isinstance(payload, dict):
        err = payload.get("error")
        if isinstance(err, dict):
            msg = err.get("message")
            if isinstance(msg, str) and msg:
                message = msg
            name = err.get("code")
            if isinstance(name, str):
                code = _grpc_code_from_name(name)
            maybe_details = err.get("details")
            if isinstance(maybe_details, list):
                details = [d for d in maybe_details if isinstance(d, dict)]

    return InvariantError(code, message, details or None)


def _grpc_code_from_http_status(status_code: int) -> grpc.StatusCode:
    mapping = {
        200: grpc.StatusCode.OK,
        499: grpc.StatusCode.CANCELLED,
        400: grpc.StatusCode.INVALID_ARGUMENT,
        504: grpc.StatusCode.DEADLINE_EXCEEDED,
        404: grpc.StatusCode.NOT_FOUND,
        409: grpc.StatusCode.ALREADY_EXISTS,
        403: grpc.StatusCode.PERMISSION_DENIED,
        429: grpc.StatusCode.RESOURCE_EXHAUSTED,
        501: grpc.StatusCode.UNIMPLEMENTED,
        500: grpc.StatusCode.INTERNAL,
        503: grpc.StatusCode.UNAVAILABLE,
        401: grpc.StatusCode.UNAUTHENTICATED,
    }
    return mapping.get(status_code, grpc.StatusCode.UNKNOWN)


def _grpc_code_from_name(name: str) -> grpc.StatusCode:
    for code in grpc.StatusCode:
        if code.name == name:
            return code
    return grpc.StatusCode.UNKNOWN


_OUTBOUND_HTTP_HEADER_ENV_PREFIX = "INVARIANT_HTTP_HEADER_"


def _outbound_http_headers_from_env() -> dict[str, str]:
    out: dict[str, str] = {}
    for key, value in os.environ.items():
        if not key.startswith(_OUTBOUND_HTTP_HEADER_ENV_PREFIX):
            continue
        if not value:
            continue

        suffix = key.removeprefix(_OUTBOUND_HTTP_HEADER_ENV_PREFIX)
        if not suffix:
            continue

        name = _env_header_suffix_to_http_header(suffix)
        if name in ("Accept", "Content-Type"):
            continue
        out[name] = value
    return out


def _env_header_suffix_to_http_header(suffix: str) -> str:
    parts = suffix.lower().split("_")
    return "-".join(part[:1].upper() + part[1:] if part else "" for part in parts)


def _retry_delay_seconds(attempt: int, retry_after: str | None) -> float:
    parsed = _parse_retry_after_seconds(retry_after)
    if parsed is not None:
        return parsed

    delay = _BASE_HTTP_RETRY_DELAY_SECONDS
    for _ in range(attempt):
        delay *= 2
        if delay >= _MAX_HTTP_RETRY_DELAY_SECONDS:
            return _MAX_HTTP_RETRY_DELAY_SECONDS
    return delay


def _parse_retry_after_seconds(value: str | None) -> float | None:
    if value is None:
        return None
    trimmed = value.strip()
    if not trimmed:
        return None

    try:
        seconds = int(trimmed)
        return float(max(0, seconds))
    except ValueError:
        pass

    try:
        dt = parsedate_to_datetime(trimmed)
    except Exception:
        return None

    if dt.tzinfo is None:
        return 0.0
    remaining = dt.timestamp() - time.time()
    return max(0.0, remaining)


def _sleep_seconds(delay: float) -> None:
    if delay > 0:
        time.sleep(delay)


def _is_safe_retry_method(method: str) -> bool:
    return method.upper() in {"GET", "HEAD"}


def _validated_base_url(base_url: str) -> str:
    parsed = urllib.parse.urlsplit(base_url)
    if parsed.scheme not in ("http", "https"):
        raise ValueError("base_url must use http:// or https://")
    if not parsed.netloc:
        raise ValueError("base_url must include host")
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path.rstrip("/"), "", ""))
