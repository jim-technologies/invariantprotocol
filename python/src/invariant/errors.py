"""Shared error helpers for projecting gRPC-style errors to all protocols."""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

import grpc

_NO_FIELD_PATTERNS = [
    re.compile(r'no field named "([^"]+)"', re.IGNORECASE),
    re.compile(r"unknown field[: ]+\"?([^\" ]+)\"?", re.IGNORECASE),
]

_HTTP_STATUS_BY_CODE = {
    grpc.StatusCode.OK: 200,
    grpc.StatusCode.CANCELLED: 499,
    grpc.StatusCode.UNKNOWN: 500,
    grpc.StatusCode.INVALID_ARGUMENT: 400,
    grpc.StatusCode.DEADLINE_EXCEEDED: 504,
    grpc.StatusCode.NOT_FOUND: 404,
    grpc.StatusCode.ALREADY_EXISTS: 409,
    grpc.StatusCode.PERMISSION_DENIED: 403,
    grpc.StatusCode.RESOURCE_EXHAUSTED: 429,
    grpc.StatusCode.FAILED_PRECONDITION: 400,
    grpc.StatusCode.ABORTED: 409,
    grpc.StatusCode.OUT_OF_RANGE: 400,
    grpc.StatusCode.UNIMPLEMENTED: 501,
    grpc.StatusCode.INTERNAL: 500,
    grpc.StatusCode.UNAVAILABLE: 503,
    grpc.StatusCode.DATA_LOSS: 500,
    grpc.StatusCode.UNAUTHENTICATED: 401,
}


@dataclass
class InvariantError(ValueError):
    """gRPC-aligned runtime error with optional structured details."""

    code: grpc.StatusCode
    message: str
    details: list[dict[str, Any]] | None = None

    def __str__(self) -> str:
        return self.message

    def to_payload(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code.name,
            "message": self.message,
        }
        if self.details:
            payload["details"] = self.details
        return payload


def as_invariant_error(err: Exception) -> InvariantError:
    if isinstance(err, InvariantError):
        return err
    return InvariantError(grpc.StatusCode.UNKNOWN, str(err))


def invalid_argument(message: str, *, field: str | None = None) -> InvariantError:
    details = None
    if field:
        details = [
            {
                "@type": "type.googleapis.com/google.rpc.BadRequest",
                "fieldViolations": [
                    {
                        "field": field,
                        "description": message,
                    }
                ],
            }
        ]
    return InvariantError(grpc.StatusCode.INVALID_ARGUMENT, message, details)


def invalid_argument_from_json_error(err: Exception) -> InvariantError:
    message = str(err)
    field = _extract_unknown_field(message)
    return invalid_argument(message, field=field)


def not_found(message: str) -> InvariantError:
    return InvariantError(grpc.StatusCode.NOT_FOUND, message)


def http_status_for(code: grpc.StatusCode) -> int:
    return _HTTP_STATUS_BY_CODE.get(code, 500)


def _extract_unknown_field(message: str) -> str | None:
    for pattern in _NO_FIELD_PATTERNS:
        match = pattern.search(message)
        if match:
            return match.group(1)
    return None
