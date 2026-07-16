from pathlib import Path

from conftest import DESCRIPTOR_PATH
from google.protobuf import descriptor_pb2

from invariant.proto_comments import check_descriptor, main


def test_proto_comment_check_passes_fixture():
    fds = descriptor_pb2.FileDescriptorSet()
    fds.ParseFromString(Path(DESCRIPTOR_PATH).read_bytes())

    result = check_descriptor(fds)

    assert result.ok
    assert result.selected_files == ("data.proto", "data_proto2.proto", "greet.proto")


def test_proto_comment_check_reports_missing_projected_comment():
    fds = _minimal_descriptor()
    file_proto = fds.file[0]
    _add_comment(file_proto, (4, 0), "Request message.")
    _add_comment(file_proto, (6, 0), "Example service.")
    _add_comment(file_proto, (6, 0, 2, 0), "Run the example RPC.")

    result = check_descriptor(fds)

    assert not result.ok
    assert result.source_info_missing == ()
    assert [item.format() for item in result.missing_comments] == ["example.proto: field example.v1.Request.name"]


def test_proto_comment_check_requires_source_info(tmp_path, capsys):
    descriptor_path = tmp_path / "descriptor.binpb"
    descriptor_path.write_bytes(_minimal_descriptor().SerializeToString())

    exit_code = main([str(descriptor_path)])

    assert exit_code == 1
    captured = capsys.readouterr()
    assert "example.proto" in captured.err
    assert "buf build -o descriptor.binpb" in captured.err


def _minimal_descriptor() -> descriptor_pb2.FileDescriptorSet:
    fds = descriptor_pb2.FileDescriptorSet()
    file_proto = fds.file.add()
    file_proto.name = "example.proto"
    file_proto.package = "example.v1"

    message_proto = file_proto.message_type.add()
    message_proto.name = "Request"
    field_proto = message_proto.field.add()
    field_proto.name = "name"
    field_proto.number = 1
    field_proto.label = descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL
    field_proto.type = descriptor_pb2.FieldDescriptorProto.TYPE_STRING

    service_proto = file_proto.service.add()
    service_proto.name = "ExampleService"
    method_proto = service_proto.method.add()
    method_proto.name = "Run"
    method_proto.input_type = ".example.v1.Request"
    method_proto.output_type = ".example.v1.Request"

    return fds


def _add_comment(file_proto: descriptor_pb2.FileDescriptorProto, path: tuple[int, ...], comment: str) -> None:
    location = file_proto.source_code_info.location.add()
    location.path.extend(path)
    location.leading_comments = comment
