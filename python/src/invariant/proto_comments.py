"""Check that projected proto elements have comments."""

from __future__ import annotations

import argparse
import fnmatch
import sys
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

from google.protobuf import descriptor_pb2

DEFAULT_EXCLUDES = ("buf/**", "google/**")


@dataclass(frozen=True)
class MissingComment:
    file: str
    kind: str
    name: str

    def format(self) -> str:
        return f"{self.file}: {self.kind} {self.name}"


@dataclass(frozen=True)
class CommentCheckResult:
    selected_files: tuple[str, ...]
    source_info_missing: tuple[str, ...]
    missing_comments: tuple[MissingComment, ...]

    @property
    def ok(self) -> bool:
        return not self.source_info_missing and not self.missing_comments


def check_descriptor(
    fds: descriptor_pb2.FileDescriptorSet,
    *,
    include: Sequence[str] = (),
    exclude: Sequence[str] = (),
) -> CommentCheckResult:
    """Return missing comment diagnostics for projected proto elements."""
    selected: list[str] = []
    source_info_missing: list[str] = []
    missing: list[MissingComment] = []
    excludes = (*DEFAULT_EXCLUDES, *exclude)

    for file_proto in fds.file:
        if not _selected(file_proto.name, include, excludes):
            continue
        selected.append(file_proto.name)
        if _has_projected_symbols(file_proto) and not file_proto.source_code_info.location:
            source_info_missing.append(file_proto.name)
            continue
        missing.extend(_missing_file_comments(file_proto))

    return CommentCheckResult(tuple(selected), tuple(source_info_missing), tuple(missing))


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Check that services, RPCs, messages, fields, enums, and enum values have proto comments."
    )
    parser.add_argument("descriptor", help="Path to descriptor.binpb")
    parser.add_argument(
        "--include",
        action="append",
        default=[],
        metavar="GLOB",
        help="Only check proto files matching this glob. May be repeated.",
    )
    parser.add_argument(
        "--exclude",
        action="append",
        default=[],
        metavar="GLOB",
        help="Skip proto files matching this glob in addition to built-in dependency excludes. May be repeated.",
    )
    args = parser.parse_args(argv)

    fds = descriptor_pb2.FileDescriptorSet()
    try:
        fds.ParseFromString(Path(args.descriptor).read_bytes())
    except OSError as exc:
        print(f"read descriptor: {exc}", file=sys.stderr)
        return 2

    result = check_descriptor(fds, include=args.include, exclude=args.exclude)
    if not result.selected_files:
        print("no proto files matched the selected include/exclude filters", file=sys.stderr)
        return 2

    if result.source_info_missing:
        print("descriptor is missing source info for selected proto files:", file=sys.stderr)
        for file_name in result.source_info_missing:
            print(f"  {file_name}", file=sys.stderr)
        print(file=sys.stderr)
        print("Rebuild it with:", file=sys.stderr)
        print("  buf build --include-source-info -o descriptor.binpb", file=sys.stderr)
        return 1

    if result.missing_comments:
        print("missing proto comments:", file=sys.stderr)
        for item in result.missing_comments:
            print(f"  {item.format()}", file=sys.stderr)
        return 1

    print(f"proto comments complete ({len(result.selected_files)} files checked)")
    return 0


def _selected(name: str, include: Sequence[str], exclude: Sequence[str]) -> bool:
    if include and not any(fnmatch.fnmatchcase(name, pattern) for pattern in include):
        return False
    return not any(fnmatch.fnmatchcase(name, pattern) for pattern in exclude)


def _has_projected_symbols(file_proto: descriptor_pb2.FileDescriptorProto) -> bool:
    return bool(file_proto.service or file_proto.message_type or file_proto.enum_type)


def _missing_file_comments(file_proto: descriptor_pb2.FileDescriptorProto) -> list[MissingComment]:
    comments = _comments_by_path(file_proto)
    package = file_proto.package
    missing: list[MissingComment] = []

    for index, enum_proto in enumerate(file_proto.enum_type):
        full_name = _full_name(package, enum_proto.name)
        missing.extend(_missing_enum_comments(file_proto.name, enum_proto, full_name, comments, (5, index)))

    for index, message_proto in enumerate(file_proto.message_type):
        full_name = _full_name(package, message_proto.name)
        missing.extend(_missing_message_comments(file_proto.name, message_proto, full_name, comments, (4, index)))

    for index, service_proto in enumerate(file_proto.service):
        service_name = _full_name(package, service_proto.name)
        _require_comment(missing, file_proto.name, "service", service_name, comments, (6, index))
        for method_index, method_proto in enumerate(service_proto.method):
            _require_comment(
                missing,
                file_proto.name,
                "rpc",
                f"{service_name}.{method_proto.name}",
                comments,
                (6, index, 2, method_index),
            )

    return missing


def _missing_message_comments(
    file_name: str,
    message_proto: descriptor_pb2.DescriptorProto,
    full_name: str,
    comments: dict[tuple[int, ...], str],
    path: tuple[int, ...],
) -> list[MissingComment]:
    if message_proto.options.map_entry:
        return []

    missing: list[MissingComment] = []
    _require_comment(missing, file_name, "message", full_name, comments, path)

    for index, field_proto in enumerate(message_proto.field):
        _require_comment(missing, file_name, "field", f"{full_name}.{field_proto.name}", comments, (*path, 2, index))

    for index, nested_proto in enumerate(message_proto.nested_type):
        nested_name = f"{full_name}.{nested_proto.name}"
        missing.extend(_missing_message_comments(file_name, nested_proto, nested_name, comments, (*path, 3, index)))

    for index, enum_proto in enumerate(message_proto.enum_type):
        enum_name = f"{full_name}.{enum_proto.name}"
        missing.extend(_missing_enum_comments(file_name, enum_proto, enum_name, comments, (*path, 4, index)))

    return missing


def _missing_enum_comments(
    file_name: str,
    enum_proto: descriptor_pb2.EnumDescriptorProto,
    full_name: str,
    comments: dict[tuple[int, ...], str],
    path: tuple[int, ...],
) -> list[MissingComment]:
    missing: list[MissingComment] = []
    _require_comment(missing, file_name, "enum", full_name, comments, path)
    for index, value_proto in enumerate(enum_proto.value):
        _require_comment(missing, file_name, "enum value", value_proto.name, comments, (*path, 2, index))
    return missing


def _require_comment(
    missing: list[MissingComment],
    file_name: str,
    kind: str,
    name: str,
    comments: dict[tuple[int, ...], str],
    path: tuple[int, ...],
) -> None:
    if not comments.get(path):
        missing.append(MissingComment(file_name, kind, name))


def _comments_by_path(file_proto: descriptor_pb2.FileDescriptorProto) -> dict[tuple[int, ...], str]:
    comments: dict[tuple[int, ...], str] = {}
    for location in file_proto.source_code_info.location:
        comment = location.leading_comments.strip()
        if not comment:
            comment = location.trailing_comments.strip()
        if comment:
            comments[tuple(location.path)] = comment
    return comments


def _full_name(package: str, name: str) -> str:
    return f"{package}.{name}" if package else name


if __name__ == "__main__":
    raise SystemExit(main())
