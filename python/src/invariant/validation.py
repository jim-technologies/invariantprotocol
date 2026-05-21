"""Protovalidate interceptor — validates requests against `buf.validate.field` constraints.

Usage::

    server.use(invariant.validation())
    server.use_stream(invariant.validation_stream())  # for server-streaming RPCs
"""

from __future__ import annotations

from typing import TYPE_CHECKING

import protovalidate

from invariant.errors import InvariantError, invalid_argument

if TYPE_CHECKING:
    from invariant.server import Interceptor, StreamInterceptor


def validation() -> Interceptor:
    """Return a unary interceptor that runs protovalidate on each request.

    Validation failures raise INVALID_ARGUMENT with field-level details.
    Requests of types without protovalidate constraints pass through unchanged.

    Streaming RPCs are not covered — pair with ``validation_stream`` and
    ``server.use_stream(vs)`` when you have streaming methods with
    protovalidate constraints.
    """
    validator = protovalidate.Validator()

    async def interceptor(request, context, info, handler):
        try:
            validator.validate(request)
        except protovalidate.ValidationError as e:
            raise _to_invariant_error(e) from None
        return await handler(request, context)

    return interceptor


def validation_stream() -> StreamInterceptor:
    """Return a stream interceptor that runs protovalidate on the request
    before opening the response stream. Failures short-circuit with
    INVALID_ARGUMENT and never produce any response messages.
    """
    validator = protovalidate.Validator()

    async def interceptor(request, context, info, handler):
        try:
            validator.validate(request)
        except protovalidate.ValidationError as e:
            raise _to_invariant_error(e) from None
        async for msg in handler(request, context):
            yield msg

    return interceptor


def _to_invariant_error(err: protovalidate.ValidationError) -> InvariantError:
    """Convert a protovalidate ValidationError into an InvariantError with field-level details."""
    field_violations = []
    for v in err.violations:
        proto_v = v.proto
        field = ".".join(el.field_name for el in proto_v.field.elements if el.field_name)
        field_violations.append({"field": field, "description": proto_v.message})

    if not field_violations:
        return invalid_argument(str(err))

    message = "; ".join(f"{fv['field']}: {fv['description']}" for fv in field_violations)
    details = [
        {
            "@type": "type.googleapis.com/google.rpc.BadRequest",
            "fieldViolations": field_violations,
        }
    ]
    return InvariantError(invalid_argument("").code, message, details)
