"""Descriptor-driven HTTP client for proxying RPC tools to REST endpoints."""

from __future__ import annotations

import asyncio
import contextlib
import json
import os
import time
import urllib.parse
from dataclasses import dataclass
from email.utils import parsedate_to_datetime
from typing import Any

import grpc
import httpx
from google.api import annotations_pb2
from google.protobuf import descriptor_pool, json_format, message_factory

from invariant.errors import InvariantError, invalid_argument

_DEFAULT_HTTP_MAX_RETRIES = 2
_BASE_HTTP_RETRY_DELAY_SECONDS = 0.1
_MAX_HTTP_RETRY_DELAY_SECONDS = 2.0
_RETRYABLE_HTTP_STATUSES = {429, 500, 502, 503, 504}
_DEFAULT_USER_AGENT = "invariant-protocol/0.1"
# Cap response bodies from upstream services we proxy through. A hostile or
# buggy upstream could otherwise stream gigabytes back and OOM us. Same
# 16 MiB shape as the inbound caps.
_HTTP_CLIENT_MAX_RESPONSE_BYTES = 16 * 1024 * 1024


@dataclass
class HTTPClientBinding:
    method: str
    pattern: str
    body: str
    response_body: str
    template: _PathTemplate

    @classmethod
    def new(cls, method: str, pattern: str, body: str, response_body: str = "") -> HTTPClientBinding:
        return cls(
            method=method.upper(),
            pattern=pattern,
            body=body,
            response_body=response_body,
            template=_PathTemplate.parse(pattern),
        )

    def resolve_fields(self, descriptor: Any) -> None:
        """Rewrite the proto-name field selectors onto the JSON names.

        `google.api.http` path variables and the `body` selector reference
        fields by their PROTO name (`{user_id}`). The request is serialized with
        the default proto3 JSON mapping, which honors `json_name`/lowerCamelCase
        — so the selectors must be translated to those JSON keys to line up.
        Done once per binding (no per-request cost). No-op without a descriptor:
        for single-word fields proto name == JSON key, and the bare unit tests
        drive `build` with proto-name dicts directly.
        """
        if descriptor is None:
            return
        for seg in self.template.segments:
            if seg.field:
                seg.field = _json_field_path(descriptor, seg.field)
        if self.body and self.body != "*":
            self.body = _json_field_path(descriptor, self.body)

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
            # Compatibility shim for descriptors that model query params inside a
            # top-level `query` struct/map instead of flattened scalar fields.
            working = _flatten_query_wrapper(working)
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
    """Async callable tool handler that proxies to a remote HTTP endpoint."""

    def __init__(
        self,
        *,
        base_url: str,
        binding: HTTPClientBinding,
        output_type: str,
        timeout: float,
        method_path: str,
        header_provider=None,
        input_type: str | None = None,
        response_observer=None,
        query_provider=None,
    ) -> None:
        self._base_url = _validated_base_url(base_url)
        self._binding = binding
        self._timeout = timeout
        self._max_retries = _DEFAULT_HTTP_MAX_RETRIES
        self._method_path = method_path
        self._header_provider = header_provider
        self._query_provider = query_provider
        self._response_observer = response_observer
        self._headers = _outbound_http_headers_from_env()
        self._client: httpx.AsyncClient | None = None
        pool = descriptor_pool.Default()
        resp_desc = pool.FindMessageTypeByName(output_type)
        self._resp_class = message_factory.GetMessageClass(resp_desc)
        # Map the request's google.api.http selectors (proto names) onto the
        # JSON keys the default proto3 mapping emits, so json_name is honored.
        if input_type:
            try:
                req_desc = pool.FindMessageTypeByName(input_type)
            except KeyError:
                req_desc = None
            binding.resolve_fields(req_desc)

    async def __call__(self, request, _context):
        # Default JSON mapping → honors json_name / lowerCamelCase on the wire.
        # The binding's selectors were translated to these JSON keys at bind time
        # (resolve_fields), so path/body/query all line up against this dict.
        args = json_format.MessageToDict(request)
        body_bytes, base_target = self._binding.build(args, self._base_url)
        attempt = 0
        client = self._ensure_client()

        while True:
            # Per attempt: query auth (e.g. HMAC signature + timestamp) and
            # header auth are recomputed so a retry re-signs with a fresh stamp.
            target = self._apply_query_provider(base_target, body_bytes)
            headers = self._build_headers(target, body_bytes)

            try:
                response = await client.request(
                    self._binding.method,
                    target,
                    content=body_bytes,
                    headers=headers,
                    timeout=self._timeout,
                )
            except httpx.RequestError as e:
                if self._should_retry(attempt, status_code=0):
                    await asyncio.sleep(_retry_delay_seconds(attempt, None))
                    attempt += 1
                    continue
                raise InvariantError(grpc.StatusCode.UNAVAILABLE, f"HTTP request failed: {e}") from None

            if response.status_code >= 400:
                if self._should_retry(attempt, status_code=response.status_code):
                    delay = _retry_delay_seconds(attempt, response.headers.get("Retry-After"))
                    await asyncio.sleep(delay)
                    attempt += 1
                    continue
                raise _http_error(response.status_code, response.content) from None

            raw = response.content
            if len(raw) > _HTTP_CLIENT_MAX_RESPONSE_BYTES:
                raise InvariantError(
                    grpc.StatusCode.RESOURCE_EXHAUSTED,
                    f"upstream HTTP response exceeds {_HTTP_CLIENT_MAX_RESPONSE_BYTES} byte limit",
                )

            self._observe_response(raw, response.status_code, target, body_bytes)

            out = self._resp_class()
            if raw.strip():
                try:
                    self._parse_http_response(raw, out)
                except Exception as e:
                    raise InvariantError(grpc.StatusCode.INTERNAL, f"decode HTTP response JSON: {e}") from None
            return out

    def _ensure_client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient()
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def _apply_query_provider(self, target: str, body_bytes: bytes | None) -> str:
        """Let an optional query_provider add auth query params (API key, HMAC
        signature + timestamp) to the URL. The provider sees the fully-built
        request (including any query the message already set) so it can sign
        over it. Mirrors the header_provider; failures surface as
        UNAUTHENTICATED.
        """
        if self._query_provider is None:
            return target
        from invariant.server import OutboundHTTPRequest

        try:
            extra = self._query_provider(
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
                f"build outbound query params for {self._method_path}: {e}",
            ) from None
        if not extra:
            return target
        parts = urllib.parse.urlsplit(target)
        params = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
        params.extend((n, v) for n, v in extra.items() if n)
        return urllib.parse.urlunsplit(
            (
                parts.scheme,
                parts.netloc,
                parts.path,
                urllib.parse.urlencode(params, doseq=True),
                parts.fragment,
            )
        )

    def _observe_response(self, raw: bytes, status_code: int, target: str, body_bytes: bytes | None) -> None:
        if self._response_observer is None:
            return
        # An observer is best-effort (archival/metrics); it must never break the
        # call path, so anything it raises is suppressed.
        with contextlib.suppress(Exception):
            from invariant.server import (
                OutboundHTTPRequest,
                OutboundHTTPResponse,
            )

            self._response_observer(
                OutboundHTTPResponse(
                    method_path=self._method_path,
                    status_code=status_code,
                    body=raw,
                    request=OutboundHTTPRequest(
                        method_path=self._method_path,
                        method=self._binding.method,
                        url=target,
                        body=body_bytes or b"",
                    ),
                )
            )

    def _parse_http_response(self, raw: bytes, out: Any) -> None:
        decoded = raw.decode()
        if self._binding.response_body and self._binding.response_body != "*":
            payload = json.loads(decoded)
            wrapped = _wrap_response_body(payload, self._binding.response_body)
            json_format.ParseDict(wrapped, out, ignore_unknown_fields=True)
            return
        json_format.Parse(decoded, out, ignore_unknown_fields=True)

    def _should_retry(self, attempt: int, *, status_code: int) -> bool:
        if attempt >= self._max_retries:
            return False
        if not _is_safe_retry_method(self._binding.method):
            return False
        if status_code == 0:
            return True
        return status_code in _RETRYABLE_HTTP_STATUSES

    def _build_headers(self, target: str, body_bytes: bytes | None) -> dict[str, str]:
        headers: dict[str, str] = {"Accept": "application/json"}
        if body_bytes is not None:
            headers["Content-Type"] = "application/json"
        headers.update(self._headers)
        self._add_dynamic_headers(headers, target, body_bytes)
        return headers

    def _add_dynamic_headers(self, headers: dict[str, str], target: str, body_bytes: bytes | None) -> None:
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
            headers[name] = value


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
    return HTTPClientBinding.new(method, pattern, rule.body, rule.response_body)


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


def _flatten_query_wrapper(args: dict[str, Any]) -> dict[str, Any]:
    query = args.get("query")
    if not isinstance(query, dict):
        return args

    merged = _clone_map(args)
    merged.pop("query", None)
    for key, value in query.items():
        if key not in merged:
            merged[key] = value
    return merged


def _wrap_response_body(payload: Any, field_path: str) -> dict[str, Any]:
    parts = [p for p in field_path.split(".") if p]
    if not parts:
        return {}
    out: Any = payload
    for part in reversed(parts):
        out = {part: out}
    return out


def _json_field_path(descriptor: Any, proto_path: str) -> str:
    """Translate a proto field path ("a.b.c", as written in google.api.http path
    templates and `body` selectors) to the path the default proto3 JSON mapping
    emits — i.e. each segment's `json_name` (an explicit `json_name` option, or
    the lowerCamelCase default). Unknown segments pass through unchanged so a
    selector that doesn't resolve degrades to its literal name rather than
    raising at bind time.
    """
    parts = proto_path.split(".")
    out: list[str] = []
    current = descriptor
    for part in parts:
        field = current.fields_by_name.get(part) if current is not None else None
        if field is None:
            out.append(part)
            current = None
            continue
        out.append(field.json_name)
        current = field.message_type  # None once we reach a scalar leaf
    return ".".join(out)


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
    """Parse a remote error envelope. Accepts both Connect-style (unwrapped,
    lowercase code) and the legacy wrapped format ({"error": {...}}, uppercase)."""
    code = _grpc_code_from_http_status(status_code)
    message = f"HTTP {status_code}"
    details: list[dict] | None = None

    try:
        payload = json.loads(body.decode() if isinstance(body, bytes) else body)
    except Exception:
        payload = {}

    if isinstance(payload, dict):
        # Connect-style: {"code": "...", "message": "...", "details": [...]}
        # Legacy wrapped: {"error": {"code": "...", ...}}
        envelope = payload
        if "code" not in envelope and isinstance(payload.get("error"), dict):
            envelope = payload["error"]

        msg = envelope.get("message")
        if isinstance(msg, str) and msg:
            message = msg
        name = envelope.get("code")
        if isinstance(name, str):
            code = _grpc_code_from_name(name)
        maybe_details = envelope.get("details")
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
    """Match either uppercase ('INVALID_ARGUMENT') or Connect lowercase ('invalid_argument')."""
    upper = name.upper()
    for code in grpc.StatusCode:
        if code.name == upper:
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
    if "User-Agent" not in out:
        out["User-Agent"] = _DEFAULT_USER_AGENT
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


def _is_safe_retry_method(method: str) -> bool:
    return method.upper() in {"GET", "HEAD"}


def _validated_base_url(base_url: str) -> str:
    parsed = urllib.parse.urlsplit(base_url)
    if parsed.scheme not in ("http", "https"):
        raise ValueError("base_url must use http:// or https://")
    if not parsed.netloc:
        raise ValueError("base_url must include host")
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path.rstrip("/"), "", ""))
