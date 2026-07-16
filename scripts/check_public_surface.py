#!/usr/bin/env python3
"""Fail if OSS-facing files contain private or product-specific references."""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

SKIP_DIRS = {
    ".git",
    ".flox",
    ".mypy_cache",
    ".pytest_cache",
    ".ruff_cache",
    ".tox",
    ".venv",
    "__pycache__",
    "dist",
    "node_modules",
    "target",
}

TEXT_SUFFIXES = {
    ".cjs",
    ".go",
    ".js",
    ".json",
    ".jsx",
    ".md",
    ".mjs",
    ".mod",
    ".proto",
    ".py",
    ".rs",
    ".sh",
    ".toml",
    ".ts",
    ".tsx",
    ".txt",
    ".yaml",
    ".yml",
}

TEXT_NAMES = {
    ".gitignore",
    ".npmignore",
    "Dockerfile",
    "Makefile",
}

SKIP_SUFFIXES = {
    ".binpb",
    ".lock",
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".webp",
    ".ico",
    ".pyc",
    ".so",
    ".dylib",
    ".dll",
    ".a",
    ".o",
    ".sum",
}

SKIP_GENERATED_SUFFIXES = (
    ".pb.go",
    "_pb2.py",
    "_pb2_grpc.py",
)

PUBLIC_PACKAGE_ALLOWLIST = (
    "@jim-technologies/invariant-protocol",
    "github.com/jim-technologies/invariantprotocol.git",
    "github.com/jim-technologies/invariantprotocol/go",
    "github.com/jim-technologies/invariantprotocol",
    "github:jim-technologies/invariantprotocol",
)

PUBLIC_PACKAGE_PATTERNS = (
    re.compile(r"@jim-technologies/invariant-protocol(?=$|[^a-z0-9_./-])"),
    re.compile(
        r"github\.com/jim-technologies/invariantprotocol/go"
        r"(?:/[a-z0-9_./-]+)?(?=$|[^a-z0-9_./-])"
    ),
    re.compile(
        r"github\.com/jim-technologies/invariantprotocol(?:\.git)?"
        r"(?=$|[^a-z0-9_./-])"
    ),
    re.compile(
        r"github:jim-technologies/invariantprotocol(?:#[a-z0-9_./-]+)?"
        r"(?=$|[^a-z0-9_./-])"
    ),
)

PRIVATE_TERMS = (
    "med" + "allion",
    "temporal" + "ess",
    "gh" + "drive",
    "jim" + "tech",
    "jim-technologies",
)

PRIVATE_PATTERNS = tuple((term, re.compile(re.escape(term), re.IGNORECASE)) for term in PRIVATE_TERMS)


def should_scan(path: Path) -> bool:
    rel = path.relative_to(ROOT)
    if rel == Path("scripts/check_public_surface.py"):
        return False
    if any(part in SKIP_DIRS for part in rel.parts):
        return False
    if path.suffix in SKIP_SUFFIXES:
        return False
    if any(path.name.endswith(suffix) for suffix in SKIP_GENERATED_SUFFIXES):
        return False
    return path.suffix in TEXT_SUFFIXES or path.name in TEXT_NAMES


def allowed(label: str, line: str) -> bool:
    if label != "jim-technologies":
        return False
    lower = line.lower()
    return any(pattern.search(lower) for pattern in PUBLIC_PACKAGE_PATTERNS)


def main() -> int:
    findings: list[str] = []
    for path in sorted(ROOT.rglob("*")):
        if not path.is_file() or not should_scan(path):
            continue
        rel = path.relative_to(ROOT)
        try:
            lines = path.read_text(encoding="utf-8").splitlines()
        except UnicodeDecodeError:
            continue
        for line_no, line in enumerate(lines, start=1):
            for label, pattern in PRIVATE_PATTERNS:
                if pattern.search(line) and not allowed(label, line):
                    findings.append(f"{rel}:{line_no}: {label}: {line.strip()}")

    if findings:
        print("Public-surface scan failed. Remove private/product-specific references.")
        print("Only public package coordinates are allowlisted: " + ", ".join(PUBLIC_PACKAGE_ALLOWLIST))
        print()
        print("\n".join(findings))
        return 1

    print("Public-surface scan passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
