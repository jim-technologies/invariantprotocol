#!/usr/bin/env python3
"""Verify Git-distributed packages share one version and cannot be published."""

from __future__ import annotations

import json
import os
import re
import sys
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SEMVER = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")


def read_json(path: str) -> dict[str, object]:
    return json.loads((ROOT / path).read_text())


def read_toml(path: str) -> dict[str, object]:
    with (ROOT / path).open("rb") as file:
        return tomllib.load(file)


def captured(path: str, pattern: str) -> str:
    match = re.search(pattern, (ROOT / path).read_text(), re.MULTILINE)
    if match is None:
        raise ValueError(f"could not find version in {path}")
    return match.group(1)


def main() -> int:
    version = (ROOT / "VERSION").read_text().strip()
    errors: list[str] = []

    if SEMVER.fullmatch(version) is None:
        errors.append(f"VERSION must be MAJOR.MINOR.PATCH, got {version!r}")

    package = read_json("package.json")
    package_lock = read_json("package-lock.json")
    pyproject = read_toml("python/pyproject.toml")
    uv_lock = read_toml("python/uv.lock")
    cargo = read_toml("rust/Cargo.toml")

    if package.get("private") is not True:
        errors.append("package.json must set private=true for Git-only distribution")
    if cargo["package"].get("publish") is not False:
        errors.append("rust/Cargo.toml must set publish=false for Git-only distribution")
    if "Private :: Do Not Upload" not in pyproject["project"].get("classifiers", []):
        errors.append("python/pyproject.toml must prohibit PyPI uploads")

    uv_projects = [
        item
        for item in uv_lock["package"]
        if item["name"] == "invariant-protocol" and item["source"] == {"editable": "."}
    ]
    if len(uv_projects) != 1:
        errors.append("python/uv.lock must contain one editable invariant-protocol package")
        uv_version = "<missing>"
    else:
        uv_version = uv_projects[0]["version"]

    actual_versions = {
        "package.json": package["version"],
        "package-lock.json": package_lock["version"],
        'package-lock.json packages[""]': package_lock["packages"][""]["version"],
        "python/pyproject.toml": pyproject["project"]["version"],
        "python/uv.lock": uv_version,
        "python source fallback": captured("python/src/invariant/version.py", r'^\s*return "([^"]+)"$'),
        "rust/Cargo.toml": cargo["package"]["version"],
        "Go server version": captured("go/server.go", r'^\s*serverVersion\s*=\s*"([^"]+)"$'),
        "TypeScript server version": captured(
            "typescript/src/server.ts", r'^export const SERVER_VERSION = "([^"]+)";$'
        ),
    }

    for source, actual in actual_versions.items():
        if actual != version:
            errors.append(f"{source} has {actual!r}; expected {version!r}")

    module_path = captured("go.mod", r"^module\s+(\S+)$")
    if module_path != "github.com/jim-technologies/invariantprotocol":
        errors.append(f"go.mod declares {module_path!r}; expected the repository root module")

    nested_go_mods: list[Path] = []
    ignored_dirs = {".flox", ".git", ".venv", "node_modules", "target"}
    for directory, dirs, files in os.walk(ROOT):
        dirs[:] = [name for name in dirs if name not in ignored_dirs]
        path = Path(directory) / "go.mod"
        if "go.mod" in files and path != ROOT / "go.mod":
            nested_go_mods.append(path.relative_to(ROOT))
    nested_go_mods.sort()
    if nested_go_mods:
        errors.append(f"nested Go modules are not allowed: {nested_go_mods}")

    nested_npm_metadata = [
        path for path in ("typescript/package.json", "typescript/package-lock.json") if (ROOT / path).exists()
    ]
    if nested_npm_metadata:
        errors.append(f"nested npm metadata duplicates the root package: {nested_npm_metadata}")

    changelog_version = captured("CHANGELOG.md", r"^## v([0-9]+\.[0-9]+\.[0-9]+)\b")
    if changelog_version != version:
        errors.append(f"latest CHANGELOG.md release is {changelog_version!r}; expected {version!r}")

    readme_install = captured("README.md", r"(?s)^## Install\s+(.*?)^## ")
    if "distributed only from Git" not in readme_install:
        errors.append("README.md must document Git-only distribution")
    readme_versions = set(re.findall(r"\bv([0-9]+\.[0-9]+\.[0-9]+)\b", readme_install))
    if readme_versions != {version}:
        errors.append(f"README.md release tags are {sorted(readme_versions)}; expected only {version!r}")

    go_install = f"go get github.com/jim-technologies/invariantprotocol/go@v{version}"
    if go_install not in readme_install:
        errors.append(f"README.md must install the Go package with {go_install!r}")

    if os.environ.get("GITHUB_REF_TYPE") == "tag":
        expected_tag = f"v{version}"
        actual_tag = os.environ.get("GITHUB_REF_NAME")
        if actual_tag != expected_tag:
            errors.append(f"release tag is {actual_tag!r}; expected {expected_tag!r}")

    if errors:
        print("version consistency check failed:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    print(f"versions aligned: {version} (tag v{version})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
