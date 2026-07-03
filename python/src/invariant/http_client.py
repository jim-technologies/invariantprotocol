"""Descriptor-driven HTTP client for proxying RPC tools to REST endpoints."""

from __future__ import annotations

import asyncio
import contextlib
import json
import os
import random
import time
import urllib.parse
from dataclasses import dataclass
from email.utils import parsedate_to_datetime
from typing import Any

import grpc
import httpx
from google.api import annotations_pb2
from google.protobuf import descriptor_pool, duration_pb2, json_format, message_factory
from google.rpc import error_details_pb2

from invariant.errors import InvariantError, invalid_argument
from invariant.version import package_version

_DEFAULT_USER_AGENT = f"invariant-protocol/{package_version()}"
# An RPC may use google.api.HttpBody as its request/response to carry a raw,
# unmodeled payload: the body bytes go in `data` and the MIME type in
# `content_type`, with no JSON<->proto mapping. Lets callers model the typed
# parts of an API and pass arbitrary/irregular payloads through verbatim.
_HTTPBODY_TYPE = "google.api.HttpBody"
_ERROR_BODY_SNIPPET_BYTES = 2048
_MAX_RETRY_ATTEMPTS = 5
_retry_jitter = random.uniform
_retry_sleep = asyncio.sleep

_GRPC_CODES_BY_NUMBER = {
    0: grpc.StatusCode.OK,
    1: grpc.StatusCode.CANCELLED,
    2: grpc.StatusCode.UNKNOWN,
    3: grpc.StatusCode.INVALID_ARGUMENT,
    4: grpc.StatusCode.DEADLINE_EXCEEDED,
    5: grpc.StatusCode.NOT_FOUND,
    6: grpc.StatusCode.ALREADY_EXISTS,
    7: grpc.StatusCode.PERMISSION_DENIED,
    8: grpc.StatusCode.RESOURCE_EXHAUSTED,
    9: grpc.StatusCode.FAILED_PRECONDITION,
    10: grpc.StatusCode.ABORTED,
    11: grpc.StatusCode.OUT_OF_RANGE,
    12: grpc.StatusCode.UNIMPLEMENTED,
    13: grpc.StatusCode.INTERNAL,
    14: grpc.StatusCode.UNAVAILABLE,
    15: grpc.StatusCode.DATA_LOSS,
    16: grpc.StatusCode.UNAUTHENTICATED,
}


@dataclass(frozen=True, slots=True)
class _RetryPolicy:
    max_attempts: int
    initial_backoff: float
    max_backoff: float
    backoff_multiplier: float
    retryable_status_codes: frozenset[grpc.StatusCode]
    retry_unsafe_methods: bool = False


@dataclass(frozen=True, slots=True)
class _MethodConfig:
    names: list[dict[str, str]]
    retry_policy: _RetryPolicy | None


@dataclass(frozen=True, slots=True)
class _ReadResult:
    body: bytes
    exceeded: bool


class HTTPConnection:
    """Shared outbound HTTP transport for one ``Server.connect_http`` call."""

    def __init__(
        self,
        *,
        base_url: str,
        auth: Any = None,
        service_config: dict[str, Any] | None = None,
        options: Any = None,
        observer: Any = None,
    ) -> None:
        from invariant.server import ChannelOptions, HTTPAuth

        self.base_url = _validated_base_url(base_url)
        if auth is None:
            self.auth = HTTPAuth()
        elif isinstance(auth, HTTPAuth):
            self.auth = auth
        elif callable(auth):
            self.auth = HTTPAuth(header_provider=auth)
        else:
            raise TypeError("auth must be None, HTTPAuth, or a callable header provider")
        self.observer = observer
        self.options = options or ChannelOptions()
        self._method_configs = _parse_method_configs(service_config)
        self._headers = _outbound_http_headers_from_env()
        self._client = httpx.AsyncClient(
            timeout=httpx.Timeout(
                connect=self.options.connect_timeout,
                read=self.options.read_timeout,
                write=self.options.write_timeout,
                pool=self.options.pool_timeout,
            ),
            limits=httpx.Limits(
                max_connections=self.options.max_connections,
                max_keepalive_connections=self.options.max_keepalive_connections,
                keepalive_expiry=self.options.keepalive_expiry,
            ),
            proxy=self.options.proxy,
            http2=self.options.http2,
        )

    @property
    def headers(self) -> dict[str, str]:
        return dict(self._headers)

    @property
    def client(self) -> httpx.AsyncClient:
        return self._client

    def retry_policy_for(self, method_path: str) -> _RetryPolicy | None:
        service, method = _split_method_path(method_path)
        best_score = -1
        best: _RetryPolicy | None = None
        for config in self._method_configs:
            for name in config.names:
                score = _method_name_match_score(name, service, method)
                if score > best_score:
                    best_score = score
                    best = config.retry_policy
        return best

    async def aclose(self) -> None:
        if not self._client.is_closed:
            await self._client.aclose()


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

        path = "/" + "/".join(parts)
        if self.template.trailing_slash:
            path += "/"
        return path, consumed

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
        connection: HTTPConnection,
        binding: HTTPClientBinding,
        output_type: str,
        method_path: str,
        input_type: str | None = None,
    ) -> None:
        self._connection = connection
        self._binding = binding
        self._method_path = method_path
        self._retry_policy = connection.retry_policy_for(method_path)
        pool = descriptor_pool.Default()
        resp_desc = pool.FindMessageTypeByName(output_type)
        self._resp_class = message_factory.GetMessageClass(resp_desc)
        self._httpbody_response = output_type == _HTTPBODY_TYPE
        self._httpbody_request = input_type == _HTTPBODY_TYPE
        # Map the request's google.api.http selectors (proto names) onto the
        # JSON keys the default proto3 mapping emits, so json_name is honored.
        if input_type:
            try:
                req_desc = pool.FindMessageTypeByName(input_type)
            except KeyError:
                req_desc = None
            binding.resolve_fields(req_desc)

    async def __call__(self, request, _context):
        req_content_type: str | None = None
        if self._httpbody_request:
            # google.api.HttpBody request: send the raw `data` bytes verbatim
            # with its declared content_type — no JSON mapping.
            body_bytes = request.data or None
            _, base_target = self._binding.build({}, self._connection.base_url)
            req_content_type = request.content_type or None
        else:
            # Default JSON mapping → honors json_name / lowerCamelCase on the
            # wire. The binding's selectors were translated to these JSON keys
            # at bind time (resolve_fields), so path/body/query all line up.
            args = json_format.MessageToDict(request)
            body_bytes, base_target = self._binding.build(args, self._connection.base_url)
        attempt = 0
        retry_backoff = self._retry_policy.initial_backoff if self._retry_policy else 0.0

        while True:
            # Per attempt: query auth (e.g. HMAC signature + timestamp) and
            # header auth are recomputed so a retry re-signs with a fresh stamp.
            target = self._apply_query_provider(base_target, body_bytes)
            headers = self._build_headers(target, body_bytes)
            if req_content_type:
                headers["Content-Type"] = req_content_type

            try:
                response, read_result, duration_ms = await self._send_once(target, body_bytes, headers)
            except httpx.RequestError as e:
                if self._should_retry(attempt, grpc.StatusCode.UNAVAILABLE):
                    retry_delay = _retry_delay_seconds(self._retry_policy, retry_backoff, None)
                    if retry_delay is None:
                        raise _request_error(self._method_path, target, e) from None
                    delay, retry_backoff = retry_delay
                    attempt += 1
                    await _retry_sleep(delay)
                    continue
                raise _request_error(self._method_path, target, e) from None

            if response.status_code >= 400:
                code = _grpc_code_from_http_status(response.status_code)
                if self._should_retry(attempt, code):
                    retry_delay = _retry_delay_seconds(
                        self._retry_policy,
                        retry_backoff,
                        response.headers.get("Retry-After"),
                    )
                    self._observe_response(response, read_result.body, duration_ms, target, body_bytes, success=False)
                    if retry_delay is None:
                        raise _http_error(
                            response.status_code,
                            read_result.body,
                            response.headers,
                            method_path=self._method_path,
                            url=target,
                            truncated=read_result.exceeded,
                        ) from None
                    delay, retry_backoff = retry_delay
                    attempt += 1
                    await _retry_sleep(delay)
                    continue
                self._observe_response(response, read_result.body, duration_ms, target, body_bytes, success=False)
                raise _http_error(
                    response.status_code,
                    read_result.body,
                    response.headers,
                    method_path=self._method_path,
                    url=target,
                    truncated=read_result.exceeded,
                ) from None

            success = not read_result.exceeded
            self._observe_response(response, read_result.body, duration_ms, target, body_bytes, success=success)
            if read_result.exceeded:
                raise _response_too_large(
                    self._connection.options.max_receive_message_size,
                    method_path=self._method_path,
                    url=target,
                    body=read_result.body,
                )

            raw = read_result.body

            if self._httpbody_response:
                # Return the raw body verbatim in a google.api.HttpBody — for
                # endpoints whose payload isn't worth modeling as a message.
                out = self._resp_class()
                out.data = raw
                content_type = response.headers.get("content-type")
                if content_type:
                    out.content_type = content_type
                return out

            out = self._resp_class()
            if raw.strip():
                try:
                    self._parse_http_response(raw, out)
                except Exception as e:
                    raise _decode_error(self._method_path, target, raw, e) from None
            return out

    async def _send_once(
        self,
        target: str,
        body_bytes: bytes | None,
        headers: dict[str, str],
    ) -> tuple[httpx.Response, _ReadResult, float]:
        start = time.perf_counter()
        async with self._connection.client.stream(
            self._binding.method,
            target,
            content=body_bytes,
            headers=headers,
            follow_redirects=self._httpbody_response,
        ) as response:
            read_result = await _read_response_body(response, self._connection.options.max_receive_message_size)
            return response, read_result, (time.perf_counter() - start) * 1000

    async def aclose(self) -> None:
        return None

    def _apply_query_provider(self, target: str, body_bytes: bytes | None) -> str:
        """Let an optional query_provider add auth query params (API key, HMAC
        signature + timestamp) to the URL. The provider sees the fully-built
        request (including any query the message already set) so it can sign
        over it. Mirrors the header_provider; failures surface as
        UNAUTHENTICATED.
        """
        if self._connection.auth.query_provider is None:
            return target
        from invariant.server import OutboundHTTPRequest

        try:
            extra = self._connection.auth.query_provider(
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

    def _observe_response(
        self,
        response: httpx.Response,
        raw: bytes,
        duration_ms: float,
        target: str,
        body_bytes: bytes | None,
        *,
        success: bool,
    ) -> None:
        if self._connection.observer is None:
            return
        # An observer is best-effort (archival/metrics); it must never break the
        # call path, so anything it raises is suppressed.
        with contextlib.suppress(Exception):
            from invariant.server import (
                OutboundHTTPRequest,
                OutboundHTTPResponse,
            )

            self._connection.observer(
                OutboundHTTPResponse(
                    method_path=self._method_path,
                    status_code=response.status_code,
                    headers=dict(response.headers),
                    body=raw,
                    duration_ms=duration_ms,
                    success=success,
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

    def _should_retry(self, attempt: int, code: grpc.StatusCode) -> bool:
        policy = self._retry_policy
        if policy is None:
            return False
        if attempt + 1 >= policy.max_attempts:
            return False
        if not _is_safe_retry_method(self._binding.method) and not policy.retry_unsafe_methods:
            return False
        return code in policy.retryable_status_codes

    def _build_headers(self, target: str, body_bytes: bytes | None) -> dict[str, str]:
        headers: dict[str, str] = {"Accept": "*/*" if self._httpbody_response else "application/json"}
        if body_bytes is not None and not self._httpbody_request:
            headers["Content-Type"] = "application/json"
        headers.update(self._connection.headers)
        self._add_dynamic_headers(headers, target, body_bytes)
        return headers

    def _add_dynamic_headers(self, headers: dict[str, str], target: str, body_bytes: bytes | None) -> None:
        if self._connection.auth.header_provider is None:
            return

        try:
            from invariant.server import OutboundHTTPRequest

            dynamic_headers = self._connection.auth.header_provider(
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


async def _read_response_body(response: httpx.Response, max_bytes: int) -> _ReadResult:
    chunks: list[bytes] = []
    total = 0
    retained = 0
    exceeded = False
    async for chunk in response.aiter_bytes():
        total += len(chunk)
        if total > max_bytes:
            remaining = max(0, max_bytes - retained)
            if remaining:
                chunks.append(chunk[:remaining])
            exceeded = True
            break
        chunks.append(chunk)
        retained += len(chunk)
    return _ReadResult(body=b"".join(chunks), exceeded=exceeded)


def _parse_method_configs(service_config: Any) -> list[_MethodConfig]:
    if service_config is None:
        return []
    if not isinstance(service_config, dict):
        raise ValueError("service_config must be a dict")

    raw_configs = _field(service_config, "method_config", "methodConfig")
    if raw_configs is None:
        return []
    if not isinstance(raw_configs, list):
        raise ValueError("service_config.method_config must be a list")

    out: list[_MethodConfig] = []
    seen_names: dict[tuple[str, str], str] = {}
    for i, raw in enumerate(raw_configs):
        if not isinstance(raw, dict):
            raise ValueError(f"service_config.method_config[{i}] must be a dict")
        if "name" not in raw:
            raise ValueError(f"service_config.method_config[{i}].name is required")
        raw_names = raw["name"]
        if not isinstance(raw_names, list) or not raw_names:
            raise ValueError(f"service_config.method_config[{i}].name must be a non-empty list")
        names: list[dict[str, str]] = []
        for j, name in enumerate(raw_names):
            path = f"service_config.method_config[{i}].name[{j}]"
            parsed_name = _parse_method_config_name(name, path)
            key = _method_config_name_key(parsed_name)
            if key in seen_names:
                raise ValueError(f"{path} duplicates {seen_names[key]}")
            seen_names[key] = path
            names.append(parsed_name)
        _retry_unsafe_methods_value(raw, f"service_config.method_config[{i}].retry_unsafe_methods")
        retry_policy = _parse_retry_policy(raw, f"service_config.method_config[{i}].retry_policy")
        out.append(_MethodConfig(names=names, retry_policy=retry_policy))
    return out


def _parse_method_config_name(raw: Any, path: str) -> dict[str, str]:
    if not isinstance(raw, dict):
        raise ValueError(f"{path} must be a dict")
    service = raw.get("service", "")
    method = raw.get("method", "")
    if not isinstance(service, str) or not isinstance(method, str):
        raise ValueError(f"{path}.service and {path}.method must be strings")
    if method and not service:
        raise ValueError(f"{path}.method requires {path}.service")
    if not service:
        return {}
    return {"service": service, "method": method}


def _method_config_name_key(name: dict[str, str]) -> tuple[str, str]:
    return name.get("service", ""), name.get("method", "")


def _parse_retry_policy(method_config: dict[str, Any], path: str) -> _RetryPolicy | None:
    raw = _field(method_config, "retry_policy", "retryPolicy")
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise ValueError(f"{path} must be a dict")

    codes = _field(raw, "retryable_status_codes", "retryableStatusCodes")
    if not isinstance(codes, list) or not codes:
        raise ValueError(f"{path}.retryable_status_codes must be a non-empty list")
    retryable_codes = [
        _parse_retryable_code(code_name, f"{path}.retryable_status_codes[{i}]") for i, code_name in enumerate(codes)
    ]
    retryable = frozenset(retryable_codes)

    max_attempts = _parse_max_attempts(_field(raw, "max_attempts", "maxAttempts"), f"{path}.max_attempts")
    initial_backoff = _duration_seconds(_field(raw, "initial_backoff", "initialBackoff"), f"{path}.initial_backoff")
    max_backoff = _duration_seconds(_field(raw, "max_backoff", "maxBackoff"), f"{path}.max_backoff")
    multiplier = _parse_backoff_multiplier(
        _field(raw, "backoff_multiplier", "backoffMultiplier"),
        f"{path}.backoff_multiplier",
    )
    retry_unsafe = _parse_retry_unsafe_methods(
        method_config,
        raw,
        f"{path.rsplit('.', 1)[0]}.retry_unsafe_methods",
        f"{path}.retry_unsafe_methods",
    )
    return _RetryPolicy(
        max_attempts=max_attempts,
        initial_backoff=initial_backoff,
        max_backoff=max_backoff,
        backoff_multiplier=multiplier,
        retryable_status_codes=retryable,
        retry_unsafe_methods=retry_unsafe,
    )


def _field(data: dict[str, Any], snake: str, camel: str) -> Any:
    if snake in data:
        return data[snake]
    return data.get(camel)


def _parse_retry_unsafe_methods(
    method_config: dict[str, Any],
    retry_policy: dict[str, Any],
    method_path: str,
    policy_path: str,
) -> bool:
    for value in (
        _retry_unsafe_methods_value(method_config, method_path),
        _retry_unsafe_methods_value(retry_policy, policy_path),
    ):
        if value is not None:
            return value
    return False


def _retry_unsafe_methods_value(data: dict[str, Any], path: str) -> bool | None:
    value = _field(data, "retry_unsafe_methods", "retryUnsafeMethods")
    if value is None:
        return None
    if isinstance(value, bool):
        return value
    raise ValueError(f"{path} must be a boolean")


def _parse_retryable_code(value: Any, path: str) -> grpc.StatusCode:
    if isinstance(value, bool):
        raise ValueError(f"{path} must be a gRPC status code string or number")
    if isinstance(value, int):
        code = _GRPC_CODES_BY_NUMBER.get(value)
        if code is None:
            raise ValueError(f"{path} has unknown gRPC status code number {value!r}")
        return code
    if not isinstance(value, str):
        raise ValueError(f"{path} must be a gRPC status code string or number")
    code = _grpc_code_from_name(value)
    if code is None:
        raise ValueError(f"{path} has unknown gRPC status code {value!r}")
    return code


def _parse_max_attempts(value: Any, path: str) -> int:
    if value is None:
        raise ValueError(f"{path} is required")
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"{path} must be an integer")
    attempts = value
    if attempts <= 1:
        raise ValueError(f"{path} must be greater than 1")
    return min(attempts, _MAX_RETRY_ATTEMPTS)


def _parse_backoff_multiplier(value: Any, path: str) -> float:
    if value is None:
        raise ValueError(f"{path} is required")
    try:
        multiplier = float(value)
    except (TypeError, ValueError) as e:
        raise ValueError(f"{path} must be a number") from e
    if multiplier <= 0:
        raise ValueError(f"{path} must be greater than 0")
    return multiplier


def _duration_seconds(value: Any, path: str) -> float:
    if value is None:
        raise ValueError(f"{path} is required")
    if isinstance(value, int | float):
        seconds = float(value)
        if seconds < 0:
            raise ValueError(f"{path} must be non-negative")
        return seconds
    if not isinstance(value, str):
        raise ValueError(f"{path} must be a duration string like '0.1s'")
    trimmed = value.strip()
    if not trimmed.endswith("s"):
        raise ValueError(f"{path} must be a duration string ending in 's'")
    trimmed = trimmed[:-1]
    try:
        seconds = float(trimmed)
    except ValueError as e:
        raise ValueError(f"{path} must be a parseable duration") from e
    if seconds < 0:
        raise ValueError(f"{path} must be non-negative")
    return seconds


def _split_method_path(method_path: str) -> tuple[str, str]:
    trimmed = method_path.strip("/")
    if "/" not in trimmed:
        return trimmed, ""
    service, method = trimmed.rsplit("/", 1)
    return service, method


def _method_name_match_score(name: dict[str, str], service: str, method: str) -> int:
    want_service = name.get("service", "")
    want_method = name.get("method", "")
    if not want_service and not want_method:
        return 0
    if want_service != service:
        return -1
    if not want_method:
        return 1
    if want_method == method:
        return 2
    return -1


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
    trailing_slash: bool = False

    @classmethod
    def parse(cls, pattern: str) -> _PathTemplate:
        if not pattern.startswith("/"):
            raise ValueError("path must start with '/'")
        # Preserve a trailing slash (e.g. "/questions/") — some APIs (Django
        # REST) 301-redirect or 404 without it.
        trailing = len(pattern) > 1 and pattern.endswith("/")
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
        return cls(segments=segments, trailing_slash=trailing)


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


def _request_error(method_path: str, url: str, err: httpx.RequestError) -> InvariantError:
    return InvariantError(
        grpc.StatusCode.UNAVAILABLE,
        f"HTTP request failed: {err}",
        [_error_info("HTTP_REQUEST_FAILED", method_path=method_path, url=url, metadata={"exception": str(err)})],
    )


def _decode_error(method_path: str, url: str, body: bytes, err: Exception) -> InvariantError:
    return InvariantError(
        grpc.StatusCode.INTERNAL,
        f"decode HTTP response JSON: {err}",
        [
            _error_info(
                "HTTP_RESPONSE_DECODE_FAILED",
                method_path=method_path,
                url=url,
                metadata={"body_snippet": _body_snippet(body)},
            )
        ],
    )


def _response_too_large(max_bytes: int, *, method_path: str, url: str, body: bytes) -> InvariantError:
    return InvariantError(
        grpc.StatusCode.RESOURCE_EXHAUSTED,
        f"upstream HTTP response exceeds {max_bytes} byte limit",
        [
            _error_info(
                "HTTP_RESPONSE_TOO_LARGE",
                method_path=method_path,
                url=url,
                metadata={"max_receive_message_size": str(max_bytes), "body_snippet": _body_snippet(body)},
            )
        ],
    )


def _http_error(
    status_code: int,
    body: bytes,
    headers: httpx.Headers,
    *,
    method_path: str,
    url: str,
    truncated: bool,
) -> InvariantError:
    """Parse a remote error envelope. Accepts both Connect-style (unwrapped,
    lowercase code) and the legacy wrapped format ({"error": {...}}, uppercase)."""
    code = _grpc_code_from_http_status(status_code)
    message = f"HTTP {status_code}"
    details: list[Any] = []

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
            code = _grpc_code_from_name(name) or grpc.StatusCode.UNKNOWN
        maybe_details = envelope.get("details")
        if isinstance(maybe_details, list):
            details.extend(d for d in maybe_details if isinstance(d, dict))

    retry_after = _parse_retry_after_seconds(headers.get("Retry-After"))
    if retry_after is not None and not _has_retry_info(details):
        details.append(_retry_info(retry_after))

    details.append(
        _error_info(
            f"HTTP_STATUS_{status_code}",
            method_path=method_path,
            url=url,
            metadata={
                "http_status": str(status_code),
                "body_snippet": _body_snippet(body),
                "body_truncated": "true" if truncated or len(body) > _ERROR_BODY_SNIPPET_BYTES else "false",
            },
        )
    )
    if code == grpc.StatusCode.RESOURCE_EXHAUSTED:
        details.append(_quota_failure(method_path, f"HTTP {status_code} from upstream"))

    return InvariantError(code, message, details)


def _retry_info(delay_seconds: float) -> error_details_pb2.RetryInfo:
    seconds = int(delay_seconds)
    nanos = int((delay_seconds - seconds) * 1_000_000_000)
    return error_details_pb2.RetryInfo(retry_delay=duration_pb2.Duration(seconds=seconds, nanos=nanos))


def _error_info(
    reason: str,
    *,
    method_path: str,
    url: str,
    metadata: dict[str, str] | None = None,
) -> error_details_pb2.ErrorInfo:
    merged = {
        "method_path": method_path,
        "url": url,
    }
    if metadata:
        merged.update(metadata)
    return error_details_pb2.ErrorInfo(
        reason=reason,
        domain="invariant.http_client",
        metadata=merged,
    )


def _quota_failure(method_path: str, description: str) -> error_details_pb2.QuotaFailure:
    return error_details_pb2.QuotaFailure(
        violations=[
            error_details_pb2.QuotaFailure.Violation(
                subject=method_path,
                description=description,
            )
        ]
    )


def _body_snippet(body: bytes) -> str:
    return body[:_ERROR_BODY_SNIPPET_BYTES].decode("utf-8", errors="replace")


def _has_retry_info(details: list[Any]) -> bool:
    for detail in details:
        if isinstance(detail, error_details_pb2.RetryInfo):
            return True
        if isinstance(detail, dict) and detail.get("@type") == "type.googleapis.com/google.rpc.RetryInfo":
            return True
    return False


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
        502: grpc.StatusCode.UNAVAILABLE,
        503: grpc.StatusCode.UNAVAILABLE,
        401: grpc.StatusCode.UNAUTHENTICATED,
    }
    return mapping.get(status_code, grpc.StatusCode.UNKNOWN)


def _grpc_code_from_name(name: Any) -> grpc.StatusCode | None:
    """Match either uppercase ('INVALID_ARGUMENT') or Connect lowercase ('invalid_argument')."""
    if isinstance(name, grpc.StatusCode):
        return name
    if not isinstance(name, str):
        return None
    upper = name.upper()
    for code in grpc.StatusCode:
        if code.name == upper:
            return code
    return None


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


def _retry_delay_seconds(
    policy: _RetryPolicy | None,
    current_backoff: float,
    retry_after: str | None,
) -> tuple[float, float] | None:
    parsed = _parse_retry_after_seconds(retry_after)
    if parsed is not None:
        if policy is not None and parsed > policy.max_backoff:
            return None
        return parsed, policy.initial_backoff if policy is not None else 0.0
    if policy is None:
        return 0.0, 0.0

    delay = _retry_jitter(0, current_backoff)
    next_backoff = min(policy.max_backoff, current_backoff * policy.backoff_multiplier)
    return delay, next_backoff


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
