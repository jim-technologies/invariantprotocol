#!/usr/bin/env python3
"""Validate the cross-language feature contract and block incomplete releases."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CONTRACT = ROOT / "conformance" / "feature-parity.json"
EXPECTED_LANGUAGES = ("go", "python", "rust", "typescript")
SUPPORT_LEVELS = {"supported", "missing", "unavailable", "not_applicable"}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--release", action="store_true", help="fail if any core feature is incomplete")
    args = parser.parse_args()
    release = args.release or os.environ.get("GITHUB_REF_TYPE") == "tag"

    contract = json.loads(CONTRACT.read_text())
    errors: list[str] = []
    gaps: list[str] = []

    if contract.get("schema_version") != 1:
        errors.append("feature contract schema_version must be 1")
    if contract.get("core_maturity") != "stable":
        errors.append("feature contract core_maturity must be stable")
    languages = tuple(contract.get("languages", ()))
    if languages != EXPECTED_LANGUAGES:
        errors.append(f"languages must be {EXPECTED_LANGUAGES}, got {languages}")

    seen: set[str] = set()
    for feature in contract.get("features", []):
        feature_id = feature.get("id")
        kind = feature.get("kind")
        if not isinstance(feature_id, str) or not feature_id:
            errors.append("every feature needs a non-empty id")
            continue
        if feature_id in seen:
            errors.append(f"duplicate feature id {feature_id!r}")
        seen.add(feature_id)
        if kind not in {"core", "ecosystem", "build_tool"}:
            errors.append(f"{feature_id}: unknown kind {kind!r}")
            continue

        if kind == "build_tool":
            if tuple(feature.get("consumers", ())) != EXPECTED_LANGUAGES:
                errors.append(f"{feature_id}: build tool must be available to every language")
            for path in feature.get("implementation", ()):
                if not (ROOT / path).exists():
                    errors.append(f"{feature_id}: implementation path does not exist: {path}")
            evidence = feature.get("tests")
            if not isinstance(evidence, list) or not evidence:
                errors.append(f"{feature_id}: build tool requires behavioral test evidence")
            else:
                for path in evidence:
                    if not isinstance(path, str) or not (ROOT / path).is_file():
                        errors.append(f"{feature_id}: test evidence does not exist: {path}")
            continue

        support = feature.get("support", {})
        tests = feature.get("tests", {})
        if set(support) != set(EXPECTED_LANGUAGES):
            errors.append(f"{feature_id}: support must name every language")
            continue
        if set(tests) != set(EXPECTED_LANGUAGES):
            errors.append(f"{feature_id}: tests must name every language")
            continue
        for language in EXPECTED_LANGUAGES:
            level = support[language]
            if level not in SUPPORT_LEVELS:
                errors.append(f"{feature_id}/{language}: invalid support level {level!r}")
            if level == "supported":
                evidence = tests[language]
                if not evidence:
                    errors.append(f"{feature_id}/{language}: supported without test evidence")
                for path in evidence:
                    if not (ROOT / path).is_file():
                        errors.append(f"{feature_id}/{language}: test evidence does not exist: {path}")
            elif kind == "core":
                gaps.append(f"{feature_id}/{language}: {level}")
        if kind == "ecosystem" and not feature.get("rationale"):
            errors.append(f"{feature_id}: ecosystem features require a rationale")

    if errors:
        print("feature parity contract is invalid:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    core = [feature for feature in contract["features"] if feature["kind"] == "core"]
    complete = sum(all(level == "supported" for level in feature["support"].values()) for feature in core)
    print(f"core feature parity: {complete}/{len(core)} complete")
    if gaps:
        print("release gaps:")
        for gap in gaps:
            print(f"- {gap}")
    if release and gaps:
        print(
            "release blocked: every core feature must support all four languages",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
