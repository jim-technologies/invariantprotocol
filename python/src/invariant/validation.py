"""Protovalidate interceptor — validates requests against `buf.validate.field` constraints.

Usage::

    server.use(invariant.validation())
"""

from __future__ import annotations

from typing import TYPE_CHECKING

import protovalidate

from invariant.errors import InvariantError, invalid_argument

if TYPE_CHECKING:
    from invariant.server import Interceptor


def validation() -> Interceptor:
    """Return an interceptor that runs protovalidate on each request.

    Validation failures raise INVALID_ARGUMENT with field-level details.
    Requests of types without protovalidate constraints pass through unchanged.
    """
    validator = protovalidate.Validator()

    async def interceptor(request, context, info, handler):
        try:
            validator.validate(request)
        except protovalidate.ValidationError as e:
            raise _to_invariant_error(e) from None
        return await handler(request, context)

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
