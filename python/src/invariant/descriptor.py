"""Parse a FileDescriptorSet and extract service/method/message info with source comments."""

from __future__ import annotations

from pathlib import Path

from google.protobuf import descriptor_pb2

from invariant.gen.invariant.v1 import types_pb2 as invpb

# Re-export proto types for backward compat
FieldInfo = invpb.FieldInfo
OneofInfo = invpb.OneofInfo
EnumValueInfo = invpb.EnumValueInfo
EnumInfo = invpb.EnumInfo
MessageInfo = invpb.MessageInfo
MethodInfo = invpb.MethodInfo
ServiceInfo = invpb.ServiceInfo


class ParsedDescriptor:
    """Parsed representation of a FileDescriptorSet with extracted comments."""

    def __init__(self, fds: descriptor_pb2.FileDescriptorSet):
        self.services: dict[str, invpb.ServiceInfo] = {}
        self.messages: dict[str, invpb.MessageInfo] = {}
        self.enums: dict[str, invpb.EnumInfo] = {}
        self._parse(fds)

    @classmethod
    def from_file(cls, path: str | Path) -> ParsedDescriptor:
        """Read a FileDescriptorSet from a binary file and return a ParsedDescriptor."""
        fds = descriptor_pb2.FileDescriptorSet()
        with open(path, "rb") as f:
            fds.ParseFromString(f.read())
        return cls(fds)

    def _parse(self, fds: descriptor_pb2.FileDescriptorSet) -> None:
        for file_proto in fds.file:
            comments = _extract_comments(file_proto)
            package = file_proto.package

            for i, enum_proto in enumerate(file_proto.enum_type):
                full_name = f"{package}.{enum_proto.name}" if package else enum_proto.name
                self.enums[full_name] = _parse_enum(enum_proto, full_name, comments, (5, i))

            for i, msg_proto in enumerate(file_proto.message_type):
                full_name = f"{package}.{msg_proto.name}" if package else msg_proto.name
                self._parse_message(msg_proto, full_name, comments, (4, i))

            for i, svc_proto in enumerate(file_proto.service):
                full_name = f"{package}.{svc_proto.name}" if package else svc_proto.name
                svc = invpb.ServiceInfo(
                    name=svc_proto.name,
                    full_name=full_name,
                    comment=comments.get(_path_key((6, i)), ""),
                )
                for j, method_proto in enumerate(svc_proto.method):
                    method = invpb.MethodInfo(
                        name=method_proto.name,
                        input_type=method_proto.input_type.lstrip("."),
                        output_type=method_proto.output_type.lstrip("."),
                        comment=comments.get(_path_key((6, i, 2, j)), ""),
                        client_streaming=method_proto.client_streaming,
                        server_streaming=method_proto.server_streaming,
                    )
                    svc.methods[method_proto.name].CopyFrom(method)
                self.services[full_name] = svc

    def _parse_message(
        self,
        msg_proto: descriptor_pb2.DescriptorProto,
        full_name: str,
        comments: dict[str, str],
        path_prefix: tuple[int, ...],
    ) -> None:
        for i, enum_proto in enumerate(msg_proto.enum_type):
            enum_full_name = f"{full_name}.{enum_proto.name}"
            self.enums[enum_full_name] = _parse_enum(enum_proto, enum_full_name, comments, (*path_prefix, 4, i))

        for i, nested_proto in enumerate(msg_proto.nested_type):
            nested_full_name = f"{full_name}.{nested_proto.name}"
            self._parse_message(nested_proto, nested_full_name, comments, (*path_prefix, 3, i))

        oneofs: list[invpb.OneofInfo] = []
        for i, oneof_proto in enumerate(msg_proto.oneof_decl):
            oneofs.append(
                invpb.OneofInfo(
                    name=oneof_proto.name,
                    comment=comments.get(_path_key((*path_prefix, 8, i)), ""),
                )
            )

        fields: list[invpb.FieldInfo] = []
        for i, field_proto in enumerate(msg_proto.field):
            has_oneof = field_proto.HasField("oneof_index")
            oneof_idx = field_proto.oneof_index if has_oneof else None
            # proto3 optional uses a synthetic oneof — treat as regular optional
            if oneof_idx is not None and field_proto.proto3_optional:
                oneof_idx = None

            field = invpb.FieldInfo(
                name=field_proto.name,
                number=field_proto.number,
                type=field_proto.type,
                type_name=field_proto.type_name.lstrip(".") if field_proto.type_name else "",
                label=field_proto.label,
                comment=comments.get(_path_key((*path_prefix, 2, i)), ""),
                optional=field_proto.proto3_optional,
            )
            if oneof_idx is not None:
                field.oneof_index = oneof_idx
            fields.append(field)

            if oneof_idx is not None and oneof_idx < len(oneofs):
                oneofs[oneof_idx].field_names.append(field.name)

        is_map_entry = msg_proto.options.map_entry if msg_proto.HasField("options") else False

        msg = invpb.MessageInfo(
            name=msg_proto.name,
            full_name=full_name,
            comment=comments.get(_path_key(path_prefix), ""),
            is_map_entry=is_map_entry,
        )
        for f in fields:
            msg.fields.append(f)
        for o in oneofs:
            msg.oneofs.append(o)

        self.messages[full_name] = msg


def _parse_enum(
    enum_proto: descriptor_pb2.EnumDescriptorProto,
    full_name: str,
    comments: dict[str, str],
    path_prefix: tuple[int, ...],
) -> invpb.EnumInfo:
    enum = invpb.EnumInfo(
        name=enum_proto.name,
        full_name=full_name,
        comment=comments.get(_path_key(path_prefix), ""),
    )
    for i, val in enumerate(enum_proto.value):
        enum.values.append(
            invpb.EnumValueInfo(
                name=val.name,
                number=val.number,
                comment=comments.get(_path_key((*path_prefix, 2, i)), ""),
            )
        )
    return enum


def _extract_comments(
    file_proto: descriptor_pb2.FileDescriptorProto,
) -> dict[str, str]:
    """Extract source_code_info comments keyed by path."""
    comments: dict[str, str] = {}
    if not file_proto.HasField("source_code_info"):
        return comments

    for location in file_proto.source_code_info.location:
        comment = location.leading_comments.strip()
        if not comment and location.trailing_comments:
            comment = location.trailing_comments.strip()
        if comment:
            comments[_path_key(tuple(location.path))] = comment

    return comments


def _path_key(path: tuple[int, ...]) -> str:
    return ",".join(str(p) for p in path)
