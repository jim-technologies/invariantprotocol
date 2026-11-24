"""Descriptor-driven HTTP client for proxying RPC tools to REST endpoints."""

from __future__ import annotations

import asyncio
import base64
import binascii
import contextlib
import contextvars
import json
import logging
import math
import os
import random
import time
import urllib.parse
from collections import Counter
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC
from email.utils import format_datetime, parsedate_to_datetime
from typing import Any, Never, cast

import grpc
import httpx
from google.api import annotations_pb2
from google.protobuf import descriptor_pool, duration_pb2, json_format, message_factory
from google.protobuf.message import DecodeError
from google.rpc import error_details_pb2

from invariant.errors import InvariantError, invalid_argument
from invariant.gen.invariant.v1.types_pb2 import HttpEntityTag
from invariant.http_types import (
    ChannelOptions,
    HTTPAuth,
    HTTPResponseObserver,
    OutboundHTTPRequest,
    OutboundHTTPResponse,
)
from invariant.version import package_version

_DEFAULT_USER_AGENT = f"invariant-protocol/{package_version()}"
# An RPC may use google.api.HttpBody as its request/response to carry a raw,
# unmodeled payload: the body bytes go in `data` and the MIME type in
# `content_type`, with no JSON<->proto mapping. Lets callers model the typed
# parts of an API and pass arbitrary/irregular payloads through verbatim.
_HTTPBODY_TYPE = "google.api.HttpBody"
_MAX_RETRY_ATTEMPTS = 5
_retry_jitter = random.uniform
_retry_sleep = asyncio.sleep

# HTTPX logs full URLs and httpcore can trace raw headers. Silence those
# logs only inside this transport's request context, including redirects.
_private_request = contextvars.ContextVar("invariant_private_http_request", default=False)


def _allow_httpx_log(_record: logging.LogRecord) -> bool:
    return not _private_request.get()


for _name in (
    "httpx",
    "httpcore.connection",
    "httpcore.http11",
    "httpcore.http2",
    "httpcore.proxy",
    "httpcore.socks",
    "hpack.hpack",
    "hpack.table",
):
    logging.getLogger(_name).addFilter(_allow_httpx_log)


_suppress_http: Callable[[], contextlib.AbstractContextManager[None]]
try:
    from opentelemetry.instrumentation.utils import suppress_http_instrumentation
except ImportError:
    _suppress_http = contextlib.nullcontext
else:
    _suppress_http = suppress_http_instrumentation


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

_GRPC_CODES_BY_CONNECT_NAME = {
    "canceled": grpc.StatusCode.CANCELLED,
    "unknown": grpc.StatusCode.UNKNOWN,
    "invalid_argument": grpc.StatusCode.INVALID_ARGUMENT,
    "deadline_exceeded": grpc.StatusCode.DEADLINE_EXCEEDED,
    "not_found": grpc.StatusCode.NOT_FOUND,
    "already_exists": grpc.StatusCode.ALREADY_EXISTS,
    "permission_denied": grpc.StatusCode.PERMISSION_DENIED,
    "resource_exhausted": grpc.StatusCode.RESOURCE_EXHAUSTED,
    "failed_precondition": grpc.StatusCode.FAILED_PRECONDITION,
    "aborted": grpc.StatusCode.ABORTED,
    "out_of_range": grpc.StatusCode.OUT_OF_RANGE,
    "unimplemented": grpc.StatusCode.UNIMPLEMENTED,
    "internal": grpc.StatusCode.INTERNAL,
    "unavailable": grpc.StatusCode.UNAVAILABLE,
    "data_loss": grpc.StatusCode.DATA_LOSS,
    "unauthenticated": grpc.StatusCode.UNAUTHENTICATED,
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
        auth: HTTPAuth | Callable[[OutboundHTTPRequest], dict[str, str] | None] | None = None,
        service_config: dict[str, Any] | None = None,
        options: ChannelOptions | None = None,
        observer: HTTPResponseObserver | None = None,
    ) -> None:
        self.base_url, self.host = _validated_base_url(base_url)
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
        if self.options.identity_response_only and (
            isinstance(self.options.max_receive_message_size, bool)
            or not isinstance(self.options.max_receive_message_size, int)
            or self.options.max_receive_message_size <= 0
        ):
            raise ValueError("identity-only response limit must be a positive integer")
        if self.options.before_http_attempt is not None and not callable(self.options.before_http_attempt):
            raise ValueError("before_http_attempt must be callable")
        self._headers = (
            _outbound_http_headers_from_env()
            if self.options.trust_env and not self.options.identity_response_only
            else {"User-Agent": _DEFAULT_USER_AGENT}
        )
        self._client: httpx.AsyncClient | None = None

    @property
    def headers(self) -> dict[str, str]:
        return dict(self._headers)

    @property
    def client(self) -> httpx.AsyncClient:
        if self._client is None:
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
                trust_env=self.options.trust_env and not self.options.identity_response_only,
            )
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
        if self._client is not None and not self._client.is_closed:
            await self._client.aclose()


@dataclass
class HTTPClientBinding:
    method: str
    pattern: str
    body: str
    response_body: str
    template: _PathTemplate
    # The canonical Connect route used when a method has no google.api.http
    # rule. A Connect unary request always carries a body and the Content-Type
    # naming its codec, even for an empty message.
    connect: bool = False

    @classmethod
    def new(
        cls,
        method: str,
        pattern: str,
        body: str,
        response_body: str = "",
        *,
        connect: bool = False,
    ) -> HTTPClientBinding:
        return cls(
            method=method.upper(),
            pattern=pattern,
            body=body,
            response_body=response_body,
            template=_PathTemplate.parse(pattern),
            connect=connect,
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
            # An empty message on the Connect route is still the JSON object
            # "{}": Connect servers answer 415 to a unary call with no
            # Content-Type, so the body is never dropped there.
            if not args and not self.connect:
                return None, "*"
            return json.dumps(args).encode(), "*"

        value, ok = _get_nested(args, self.body)
        if not ok:
            return None, self.body
        return json.dumps(value).encode(), self.body


class _AdmittedAttemptTimeout(httpx.TimeoutException):
    """A local attempt budget elapsed, with unknown connection progress."""


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
        pool: descriptor_pool.DescriptorPool,
    ) -> None:
        self._connection = connection
        self._binding = binding
        self._method_path = method_path
        self._retry_policy = connection.retry_policy_for(method_path)
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

    async def __call__(self, request, context):
        failure: InvariantError | type[Exception]
        try:
            return await self._call(request, context)
        except InvariantError as error:
            failure = error
        except Exception as error:
            failure = type(error)
        if isinstance(failure, type):
            failure = _request_error(self._connection.host, failure)
        return _raise_without_chain(failure)

    async def _call(self, request, context):
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
        time_remaining = getattr(context, "time_remaining", lambda: None)
        attempt = 0
        retry_backoff = self._retry_policy.initial_backoff if self._retry_policy else 0.0

        while True:
            # Per attempt: query auth (e.g. HMAC signature + timestamp) and
            # header auth are recomputed so a retry re-signs with a fresh stamp.
            target = self._apply_query_provider(base_target, body_bytes)
            headers = self._build_headers(target, body_bytes)
            if req_content_type:
                headers["Content-Type"] = req_content_type

            error: InvariantError | None = None
            failure_type: type[Exception] | None = None
            effective_host = self._connection.host
            try:
                token = _private_request.set(True)
                try:
                    remaining = time_remaining()
                    if remaining is not None and remaining <= 0:
                        raise _AdmittedAttemptTimeout("HTTP call deadline elapsed")
                    budget = asyncio.timeout(remaining)
                    try:
                        with _suppress_http():
                            async with budget:
                                response, read_result, duration_ms = await self._send_once(
                                    target, body_bytes, headers, base_target=base_target
                                )
                    except TimeoutError:
                        if not budget.expired():
                            raise
                        raise _AdmittedAttemptTimeout("HTTP call deadline elapsed") from None
                finally:
                    _private_request.reset(token)
            except _ResponsePolicyError as exc:
                error = exc
            except Exception as exc:
                failure_type = type(exc)
                if isinstance(exc, httpx.RequestError):
                    with contextlib.suppress(RuntimeError):
                        effective_host = exc.request.url.host
            if failure_type is not None:
                error = _request_error(effective_host, failure_type)
            if error is not None:
                if self._should_retry(attempt, error.code):
                    retry_delay = _retry_delay_seconds(self._retry_policy, retry_backoff, None)
                    if retry_delay is not None and _retry_fits(retry_delay[0], time_remaining()):
                        delay, retry_backoff = retry_delay
                        attempt += 1
                        await _retry_sleep(delay)
                        continue
                raise error from None

            effective_host = response.url.host
            bounded_body_exceeded = self._connection.options.identity_response_only and read_result.exceeded
            response_failed = response.status_code >= 400 or (
                response.is_redirect and not _is_safe_retry_method(self._binding.method)
            )
            if response_failed and not bounded_body_exceeded:
                response_error, pushback = _http_error(
                    response.status_code,
                    read_result.body,
                    response.headers,
                    host=effective_host,
                    connect_error_reasons=self._connection.options.connect_error_reasons,
                )
                refused = self._is_response_refusal(response, read_result)
                if refused:
                    response_error = InvariantError(
                        response_error.code,
                        str(response_error),
                        [
                            _error_info(
                                "HTTP_PROVIDER_REFUSAL",
                                effective_host,
                                http_status=str(response.status_code),
                            )
                        ],
                    )
                if not refused and self._should_retry(attempt, response_error.code):
                    retry_delay = _retry_delay_seconds(
                        self._retry_policy,
                        retry_backoff,
                        pushback,
                    )
                    self._observe_response(response, read_result.body, duration_ms, target, body_bytes, success=False)
                    if retry_delay is None or not _retry_fits(retry_delay[0], time_remaining()):
                        raise response_error from None
                    delay, retry_backoff = retry_delay
                    attempt += 1
                    await _retry_sleep(delay)
                    continue
                self._observe_response(response, read_result.body, duration_ms, target, body_bytes, success=False)
                raise response_error from None

            if read_result.exceeded:
                self._observe_response(response, b"", duration_ms, target, body_bytes, success=False)
                raise _response_too_large(
                    self._connection.options.max_receive_message_size,
                    host=effective_host,
                )

            raw = read_result.body

            if self._httpbody_response:
                # Return the raw body verbatim in a google.api.HttpBody — for
                # endpoints whose payload isn't worth modeling as a message.
                out = self._resp_class()
                http_body = cast(Any, out)
                http_body.data = raw
                content_type = response.headers.get("content-type")
                if content_type:
                    http_body.content_type = content_type
                etag = _response_etag(response.headers)
                if etag is not None:
                    http_body.extensions.add().Pack(HttpEntityTag(etag=etag))
                self._observe_response(response, raw, duration_ms, target, body_bytes, success=True)
                return out

            out = self._resp_class()
            decode_failed = False
            if raw.strip():
                try:
                    self._parse_http_response(raw, out)
                except Exception:
                    decode_failed = True
            if decode_failed:
                self._observe_response(response, b"", duration_ms, target, body_bytes, success=False)
                raise _decode_error(effective_host) from None
            self._observe_response(response, raw, duration_ms, target, body_bytes, success=True)
            return out

    def _is_response_refusal(self, response: httpx.Response, result: _ReadResult) -> bool:
        classify = self._connection.options.classify_response_refusal
        if classify is None or response.status_code < 400 or result.exceeded or len(result.body) > 4096:
            return False
        with contextlib.suppress(Exception):
            return classify(self._method_path, response.status_code, result.body) is True
        return False

    def _follow_redirects(self) -> bool:
        """Resolve the connection redirect policy for this method's response type."""
        if not _is_safe_retry_method(self._binding.method) and (
            self._retry_policy is None or not self._retry_policy.retry_unsafe_methods
        ):
            return False
        configured = self._connection.options.follow_redirects
        return self._httpbody_response if configured is None else configured

    async def _send_once(
        self,
        target: str,
        body_bytes: bytes | None,
        headers: dict[str, str],
        *,
        base_target: str,
    ) -> tuple[httpx.Response, _ReadResult, float]:
        """Keep redirect reads and each physical admission inside the limits."""
        start = time.perf_counter()
        client = self._connection.client
        request = client.build_request(self._binding.method, target, content=body_bytes, headers=headers)
        redirects = 0
        crossed_origin = False
        source_query_auth: set[tuple[str, str]] = set()
        if self._connection.auth.query_provider is not None or client.params:
            # Capture additions once. A Location may echo an auth value from
            # the initial request or from any later hop.
            source_query_auth = set(
                Counter(request.url.params.multi_items()) - Counter(httpx.URL(base_target).params.multi_items())
            )
        while True:
            if self._connection.options.identity_response_only:
                request.headers["Accept-Encoding"] = "identity"
            callback = self._connection.options.before_http_attempt
            seconds = callback() if callback is not None else None
            if seconds is not None and (
                isinstance(seconds, bool)
                or not isinstance(seconds, (int, float))
                or not math.isfinite(seconds)
                or seconds <= 0
            ):
                raise ValueError("HTTP attempt admission needs a finite positive timeout")
            budget = asyncio.timeout(seconds)
            try:
                async with budget:
                    if crossed_origin:
                        # HTTPX's public send runs client-wide hooks and auth.
                        # Keep the selected transport but bypass those source
                        # policies for an explicitly allowed off-origin hop.
                        request.extensions["timeout"] = client.timeout.as_dict()
                        transport = client._transport_for_url(request.url)
                        # The AsyncClient single-send helper extracts cookies
                        # into its jar. Borrow only the transport so no
                        # off-origin response can modify source cookie state.
                        try:
                            response = await transport.handle_async_request(request)
                        except httpx.RequestError as error:
                            error.request = request
                            raise
                        response.request = request
                        response.default_encoding = client._default_encoding
                    else:
                        response = await client.send(request, stream=True, follow_redirects=False)
                    try:
                        result = await _read_response_body(
                            response,
                            self._connection.options.max_receive_message_size,
                            identity_only=self._connection.options.identity_response_only,
                        )
                    finally:
                        await response.aclose()
            except TimeoutError:
                if not budget.expired():
                    raise
                raise _AdmittedAttemptTimeout("admitted HTTP attempt deadline elapsed", request=request) from None
            if result.exceeded:
                return response, result, (time.perf_counter() - start) * 1000
            next_request = None
            if self._follow_redirects():
                if crossed_origin and response.has_redirect_location:
                    next_request = client._build_redirect_request(request, response)
                else:
                    next_request = response.next_request
            if next_request is None:
                return response, result, (time.perf_counter() - start) * 1000
            redirects += 1
            if redirects > client.max_redirects:
                raise httpx.TooManyRedirects("admitted HTTP redirect limit exceeded", request=request)
            source = request.url
            dest = next_request.url
            if (source.scheme, source.host, source.port) != (dest.scheme, dest.host, dest.port):
                if (
                    not self._connection.options.allow_cross_origin_redirects
                    or dest.scheme not in {"http", "https"}
                    or not _is_safe_retry_method(request.method)
                    or not _is_safe_retry_method(next_request.method)
                ):
                    raise _ResponsePolicyError(
                        grpc.StatusCode.FAILED_PRECONDITION,
                        "cross-origin HTTP redirect refused",
                        [_error_info("HTTP_REDIRECT_REFUSED", source.host)],
                    )
                crossed_origin = True
            if crossed_origin:
                # Start over with a plain request. The client's normal build
                # path would merge configured headers, auth and its cookie jar.
                request = httpx.Request(
                    next_request.method,
                    dest.copy_with(
                        username=None,
                        password=None,
                        query=_redirect_query(dest.query, source_query_auth),
                        fragment=None,
                    ),
                    headers={"Accept": "*/*" if self._httpbody_response else "application/json"},
                )
            else:
                request = next_request

    def _apply_query_provider(self, target: str, body_bytes: bytes | None) -> str:
        """Let an optional query_provider add auth query params (API key, HMAC
        signature + timestamp) to the URL. The provider sees the fully-built
        request (including any query the message already set) so it can sign
        over it. Mirrors the header_provider; failures surface as
        UNAUTHENTICATED.
        """
        if self._connection.auth.query_provider is None:
            return target
        auth_failure = None
        try:
            extra = self._connection.auth.query_provider(
                OutboundHTTPRequest(
                    method_path=self._method_path,
                    method=self._binding.method,
                    url=target,
                    body=body_bytes or b"",
                )
            )
        except Exception as error:
            auth_failure = type(error)
        if auth_failure is not None:
            raise _auth_error(self._connection.host, auth_failure) from None
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
            metadata = _observer_metadata(response.headers) if success else {}
            self._connection.observer(
                OutboundHTTPResponse(
                    method_path=self._method_path,
                    status_code=response.status_code,
                    headers=dict(metadata),
                    metadata=metadata,
                    body=raw if success else b"",
                    duration_ms=duration_ms,
                    success=success,
                    request=OutboundHTTPRequest(
                        method_path=self._method_path,
                        method=self._binding.method,
                        url=self._observer_url(target, success),
                        body=(body_bytes or b"") if success else b"",
                    ),
                    url=self._observer_url(str(response.url), success),
                )
            )

    def _observer_url(self, url: str, success: bool) -> str:
        parsed = httpx.URL(url)
        if not success or self._connection.auth.query_provider is not None or parsed.userinfo:
            return _origin(url)
        return str(parsed.copy_with(query=None, fragment=None))

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

        auth_failure = None
        try:
            dynamic_headers = self._connection.auth.header_provider(
                OutboundHTTPRequest(
                    method_path=self._method_path,
                    method=self._binding.method,
                    url=target,
                    body=body_bytes or b"",
                )
            )
        except Exception as error:
            auth_failure = type(error)
        if auth_failure is not None:
            raise _auth_error(self._connection.host, auth_failure) from None

        if not dynamic_headers:
            return
        for name, value in dynamic_headers.items():
            if not name or not value:
                continue
            lowered = name.lower()
            # The codec owns Accept and Content-Type.
            if lowered in ("accept", "content-type"):
                continue
            headers[name] = value


class _ResponsePolicyError(InvariantError):
    """An error already built from trusted transport policy data."""


async def _read_response_body(
    response: httpx.Response,
    max_bytes: int,
    *,
    identity_only: bool = False,
) -> _ReadResult:
    if identity_only:
        encoding = response.headers.get("content-encoding", "identity").strip().lower()
        if encoding != "identity":
            raise _ResponsePolicyError(
                grpc.StatusCode.FAILED_PRECONDITION,
                "identity-only HTTP response uses an unsupported encoding",
            )
        declared = response.headers.get("content-length")
        if declared is not None:
            if not declared.isascii() or not declared.isdigit():
                raise _ResponsePolicyError(
                    grpc.StatusCode.FAILED_PRECONDITION,
                    "identity-only HTTP Content-Length is invalid",
                )
            significant = declared.lstrip("0") or "0"
            ceiling = str(max_bytes)
            if len(significant) > len(ceiling) or (len(significant) == len(ceiling) and significant > ceiling):
                return _ReadResult(body=b"", exceeded=True)
        raw_chunks: list[bytes] = []
        retained = 0
        async for chunk in response.aiter_raw():
            if len(chunk) > max_bytes - retained:
                return _ReadResult(body=b"".join(raw_chunks), exceeded=True)
            raw_chunks.append(chunk)
            retained += len(chunk)
        return _ReadResult(body=b"".join(raw_chunks), exceeded=False)
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
        method_config = cast(dict[str, Any], raw)
        if "name" not in method_config:
            raise ValueError(f"service_config.method_config[{i}].name is required")
        raw_names = method_config["name"]
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
        _retry_unsafe_methods_value(method_config, f"service_config.method_config[{i}].retry_unsafe_methods")
        retry_policy = _parse_retry_policy(method_config, f"service_config.method_config[{i}].retry_policy")
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
        return HTTPClientBinding.new("POST", f"/{service_full_name}/{method_name}", "*", connect=True)

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


def _redirect_query(query: bytes, source_query_auth: set[tuple[str, str]]) -> bytes | None:
    """Keep destination query bytes, except for echoed source auth pairs."""
    if not query or not source_query_auth:
        return query or None
    fields = []
    for field in query.split(b"&"):
        pairs = urllib.parse.parse_qsl(field.decode("ascii"), keep_blank_values=True)
        if not any(pair in source_query_auth for pair in pairs):
            fields.append(field)
    return b"&".join(fields) or None


def _response_etag(headers: httpx.Headers) -> str | None:
    values = headers.get_list("etag", split_commas=False)
    if len(values) != 1:
        return None
    value = values[0]
    opaque = value.removeprefix("W/")
    if (
        len(opaque) >= 2
        and opaque[0] == opaque[-1] == '"'
        and all(char == "!" or "#" <= char <= "~" or "\x80" <= char <= "\xff" for char in opaque[1:-1])
    ):
        return value
    return None


def _observer_metadata(headers: httpx.Headers) -> dict[str, str]:
    # Typed protocol data needed by existing archival and pagination clients.
    metadata = {}
    etag = _response_etag(headers)
    if etag is not None:
        metadata["etag"] = etag
    for name in (
        "record-total",
        "record-offset",
        "record-limit",
        "total-records-on-page",
        "record-max-limit",
        "x-oddsapi-seq",
    ):
        value = headers.get(name, "")
        if len(value) <= 32 and value.isascii() and value.isdecimal():
            metadata[name] = str(int(value))
    media = headers.get("content-type", "").split(";", 1)[0].lower()
    if media in {
        "application/json",
        "text/csv",
        "text/html",
        "text/plain",
        "application/xml",
        "text/xml",
        "application/octet-stream",
    }:
        metadata["content-type"] = media
    try:
        modified = parsedate_to_datetime(headers.get("last-modified", ""))
    except ValueError, TypeError, OverflowError:
        pass
    else:
        if modified.tzinfo is not None:
            metadata["last-modified"] = format_datetime(modified.astimezone(UTC), usegmt=True)
    return metadata


def _origin(url: str) -> str:
    parsed = httpx.URL(url)
    return str(parsed.copy_with(username=None, password=None, path="/", query=None, fragment=None))


def _raise_without_chain(failure: Exception) -> Never:
    # An awaiting caller may itself be handling an exception. Clear even
    # that implicit context after Python has installed it on the raise.
    try:
        raise failure from None
    except Exception:
        failure.__context__ = None
        failure.__cause__ = None
        raise


_TEXT_SAFE_ERROR_TYPES = (
    RuntimeError,
    ValueError,
    TimeoutError,
    UnicodeError,
    UnicodeEncodeError,
    httpx.InvalidURL,
    httpx.LocalProtocolError,
    httpx.RemoteProtocolError,
    httpx.PoolTimeout,
    httpx.ReadTimeout,
    httpx.WriteTimeout,
    httpx.ConnectTimeout,
    httpx.ConnectError,
    httpx.ReadError,
    httpx.WriteError,
    httpx.TooManyRedirects,
    httpx.DecodingError,
    _AdmittedAttemptTimeout,
)


def _error_class(error_type: type[Exception]) -> str:
    for known in _TEXT_SAFE_ERROR_TYPES:
        if error_type is known:
            return known.__name__
    return "HTTPExecutionError"


def _auth_error(host: str, error_type: type[Exception]) -> InvariantError:
    reason = "HTTP_AUTH_BUILD_FAILED"
    return InvariantError(
        grpc.StatusCode.UNAUTHENTICATED, reason, [_error_info(reason, host, error_class=_error_class(error_type))]
    )


def _request_error(host: str, error_type: type[Exception]) -> InvariantError:
    if issubclass(error_type, (httpx.ReadTimeout, httpx.WriteTimeout)):
        reason, code = "REMOTE_TIMEOUT", grpc.StatusCode.DEADLINE_EXCEEDED
    elif issubclass(error_type, (httpx.ReadError, httpx.WriteError, httpx.RemoteProtocolError)):
        reason, code = "REMOTE_TRANSPORT", grpc.StatusCode.UNAVAILABLE
    elif issubclass(error_type, (httpx.PoolTimeout, _AdmittedAttemptTimeout)):
        reason, code = "LOCAL_HTTP_FAILURE", grpc.StatusCode.DEADLINE_EXCEEDED
    elif issubclass(error_type, httpx.ConnectTimeout):
        reason, code = "CONNECT_TIMEOUT", grpc.StatusCode.DEADLINE_EXCEEDED
    elif issubclass(error_type, httpx.ConnectError):
        reason, code = "CONNECT_FAILED", grpc.StatusCode.UNAVAILABLE
    elif issubclass(error_type, httpx.LocalProtocolError):
        reason, code = "LOCAL_HTTP_FAILURE", grpc.StatusCode.UNAVAILABLE
    else:
        reason, code = "LOCAL_HTTP_FAILURE", grpc.StatusCode.INTERNAL
    return InvariantError(code, reason, [_error_info(reason, host, error_class=_error_class(error_type))])


def _decode_error(host: str) -> InvariantError:
    reason = "HTTP_RESPONSE_DECODE_FAILED"
    return InvariantError(grpc.StatusCode.INTERNAL, "decode HTTP response JSON", [_error_info(reason, host)])


def _response_too_large(max_bytes: int, *, host: str) -> InvariantError:
    return _ResponsePolicyError(
        grpc.StatusCode.RESOURCE_EXHAUSTED,
        f"upstream HTTP response exceeds {max_bytes} byte limit",
        [_error_info("HTTP_RESPONSE_TOO_LARGE", host)],
    )


def _http_error(
    status_code: int,
    body: bytes,
    headers: httpx.Headers,
    *,
    host: str,
    connect_error_reasons: frozenset[tuple[str, str]] = frozenset(),
) -> tuple[InvariantError, float | None]:
    try:
        payload = json.loads(body)
    except ValueError, RecursionError:
        payload = None
    connect_code = _grpc_code_from_connect_name(payload.get("code")) if isinstance(payload, dict) else None
    error_class = "CONNECT_APPLICATION" if connect_code is not None else "HTTP_RESPONSE"
    metadata = {"http_status": str(status_code), "error_class": error_class}
    retry_after = _parse_retry_after_seconds(headers.get("Retry-After"))
    details = []
    if retry_after is not None:
        metadata["retry_after"] = str(int(retry_after)) if retry_after.is_integer() else str(retry_after)
    delay = _connect_retry_seconds(payload) if connect_code is not None else None
    if delay is None:
        delay = retry_after
    if delay is not None:
        details.append(_retry_info(delay))
    code = connect_code or _grpc_code_from_http_status(status_code)
    if code == grpc.StatusCode.RESOURCE_EXHAUSTED or (connect_code is not None and _connect_has_quota(payload)):
        # Preserve the quota signal without forwarding provider-supplied
        # violation subjects or descriptions, which can contain credentials.
        details.append(error_details_pb2.QuotaFailure())
    if connect_code is not None and connect_error_reasons:
        details.extend(_connect_error_info(payload, connect_error_reasons))
    details.append(_error_info(f"HTTP_STATUS_{status_code}", host, **metadata))
    return InvariantError(code, f"HTTP {status_code}", details), delay


def _connect_error_info(payload: dict, allowed: frozenset[tuple[str, str]]) -> list[error_details_pb2.ErrorInfo]:
    details = payload.get("details")
    if not isinstance(details, list):
        return []
    matched = set()
    for detail in details:
        if not isinstance(detail, dict):
            continue
        if detail.get("type") == "google.rpc.ErrorInfo":
            encoded = detail.get("value")
            if not isinstance(encoded, str) or len(encoded) > 4096:
                continue
            try:
                wire = base64.b64decode(encoded + "=" * (-len(encoded) % 4), validate=True)
                info = error_details_pb2.ErrorInfo.FromString(wire)
            except ValueError, binascii.Error, DecodeError:
                continue
            domain, reason = info.domain, info.reason
        elif detail.get("@type") == "type.googleapis.com/google.rpc.ErrorInfo":
            domain, reason = detail.get("domain"), detail.get("reason")
        else:
            continue
        if isinstance(domain, str) and isinstance(reason, str) and (domain, reason) in allowed:
            matched.add((domain, reason))
    return [error_details_pb2.ErrorInfo(domain=domain, reason=reason) for domain, reason in sorted(matched)]


def _connect_has_quota(payload: dict) -> bool:
    details = payload.get("details")
    return isinstance(details, list) and any(
        isinstance(detail, dict)
        and (
            detail.get("type") == "google.rpc.QuotaFailure"
            or detail.get("@type") == "type.googleapis.com/google.rpc.QuotaFailure"
        )
        for detail in details
    )


def _connect_retry_seconds(payload: dict) -> float | None:
    """Read a numeric duration, never forward the remote detail itself."""
    details = payload.get("details")
    if not isinstance(details, list):
        return None
    for detail in details:
        if not isinstance(detail, dict):
            continue
        if detail.get("type") == "google.rpc.RetryInfo":
            encoded = detail.get("value")
            if not isinstance(encoded, str) or len(encoded) > 128:
                continue
            try:
                serialized = base64.b64decode(encoded + "=" * (-len(encoded) % 4), validate=True)
                retry = error_details_pb2.RetryInfo.FromString(serialized)
            except ValueError, binascii.Error, DecodeError:
                continue
            if not retry.HasField("retry_delay"):
                continue
            duration = retry.retry_delay
        elif detail.get("@type") == "type.googleapis.com/google.rpc.RetryInfo":
            value = detail.get("retryDelay")
            if not isinstance(value, str) or len(value) > 32:
                continue
            duration = duration_pb2.Duration()
            try:
                duration.FromJsonString(value)
            except ValueError:
                continue
        else:
            continue
        if 0 <= duration.seconds <= 315_576_000_000 and 0 <= duration.nanos < 1_000_000_000:
            delay = duration.seconds + duration.nanos / 1_000_000_000
            if delay <= 315_576_000_000:
                return delay
    return None


def _retry_info(delay_seconds: float) -> error_details_pb2.RetryInfo:
    seconds = int(delay_seconds)
    nanos = int((delay_seconds - seconds) * 1_000_000_000)
    return error_details_pb2.RetryInfo(retry_delay=duration_pb2.Duration(seconds=seconds, nanos=nanos))


def _error_info(reason: str, host: str, **metadata: str) -> error_details_pb2.ErrorInfo:
    return error_details_pb2.ErrorInfo(
        reason=reason,
        domain="invariant.http_client",
        metadata={"host": host, **metadata},
    )


def _grpc_code_from_http_status(status_code: int) -> grpc.StatusCode:
    if status_code in {502, 503, 504, 520, 521, 522, 523, 524}:
        return grpc.StatusCode.UNAVAILABLE
    mapping = {
        400: grpc.StatusCode.INTERNAL,
        401: grpc.StatusCode.UNAUTHENTICATED,
        403: grpc.StatusCode.PERMISSION_DENIED,
        404: grpc.StatusCode.UNIMPLEMENTED,
        429: grpc.StatusCode.RESOURCE_EXHAUSTED,
    }
    return mapping.get(status_code, grpc.StatusCode.UNKNOWN)


def _grpc_code_from_name(name: Any) -> grpc.StatusCode | None:
    """Match a standard uppercase gRPC status name."""
    if isinstance(name, grpc.StatusCode):
        return name
    if not isinstance(name, str):
        return None
    for code in grpc.StatusCode:
        if code.name == name:
            return code
    return None


def _grpc_code_from_connect_name(name: Any) -> grpc.StatusCode | None:
    """Match a canonical lowercase Connect status name."""
    if not isinstance(name, str):
        return None
    return _GRPC_CODES_BY_CONNECT_NAME.get(name)


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


def _retry_fits(delay: float, remaining: float | None) -> bool:
    return remaining is None or delay < remaining


def _retry_delay_seconds(
    policy: _RetryPolicy | None,
    current_backoff: float,
    pushback: float | None,
) -> tuple[float, float] | None:
    if pushback is not None:
        if policy is not None and pushback > policy.max_backoff:
            return None
        return pushback, policy.initial_backoff if policy is not None else 0.0
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

    if trimmed.isascii() and trimmed.isdecimal():
        if len(trimmed) > 12:
            return None
        seconds = int(trimmed)
        return float(seconds) if seconds <= 315_576_000_000 else None

    try:
        dt = parsedate_to_datetime(trimmed)
    except Exception:
        return None

    if dt.tzinfo is None:
        return None
    remaining = dt.timestamp() - time.time()
    return min(315_576_000_000.0, max(0.0, remaining))


def _is_safe_retry_method(method: str) -> bool:
    return method.upper() in {"GET", "HEAD"}


def validate_http_base_url(base_url: str) -> None:
    """Validate an outbound HTTP origin without constructing a connection."""
    _validated_base_url(base_url)


def _validated_base_url(base_url: str) -> tuple[str, str]:
    invalid = False
    try:
        parsed = urllib.parse.urlsplit(base_url)
        # Access port explicitly: urlsplit defers numeric/range validation.
        _ = parsed.port
        url = httpx.URL(base_url)
        if url.scheme not in {"http", "https"} or not url.host:
            invalid = True
    except ValueError, httpx.InvalidURL:
        invalid = True
    if invalid:
        _raise_without_chain(ValueError("base_url must be an HTTP(S) URL with a valid host and port"))
    normalized = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path.rstrip("/"), "", ""))
    return normalized, url.host
