"""Package version helpers."""

from __future__ import annotations

from importlib.metadata import PackageNotFoundError, version


def package_version() -> str:
    try:
        return version("invariant-protocol")
    except PackageNotFoundError:
        return "0.6.0"
