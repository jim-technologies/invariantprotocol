import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { Code as ConnectCode, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "vitest";

import {
  asInvariantError,
  codeFromConnectCode,
  codeFromGrpcStatus,
  codeFromHttpStatus,
  connectCodeFor,
  grpcStatusFor,
  httpStatusFor,
  InvariantError,
  toConnectError,
  type Code,
} from "../src/errors.js";
import { GreetResponseSchema } from "./gen/greet_pb.js";

describe("canonical status conversions", () => {
  test("maps every Invariant code to its gRPC, Connect, and HTTP representations", () => {
    const matrix: Array<[Code, number, number]> = [
      ["canceled", 1, 499],
      ["unknown", 2, 500],
      ["invalid_argument", 3, 400],
      ["deadline_exceeded", 4, 504],
      ["not_found", 5, 404],
      ["already_exists", 6, 409],
      ["permission_denied", 7, 403],
      ["resource_exhausted", 8, 429],
      ["failed_precondition", 9, 400],
      ["aborted", 10, 409],
      ["out_of_range", 11, 400],
      ["unimplemented", 12, 501],
      ["internal", 13, 500],
      ["unavailable", 14, 503],
      ["data_loss", 15, 500],
      ["unauthenticated", 16, 401],
    ];

    for (const [code, grpcStatus, httpStatus] of matrix) {
      expect(grpcStatusFor(code)).toBe(grpcStatus);
      expect(codeFromGrpcStatus(grpcStatus)).toBe(code);
      expect(connectCodeFor(code)).toBe(grpcStatus);
      expect(codeFromConnectCode(grpcStatus as ConnectCode)).toBe(code);
      expect(httpStatusFor(code)).toBe(httpStatus);
    }

    expect([400, 401, 403, 404, 429, 502, 503, 504, 200, 409, 418, 499, 500, 501].map(codeFromHttpStatus)).toEqual([
      "internal",
      "unauthenticated",
      "permission_denied",
      "unimplemented",
      "unavailable",
      "unavailable",
      "unavailable",
      "unavailable",
      "unknown",
      "unknown",
      "unknown",
      "unknown",
      "unknown",
      "unknown",
    ]);
    expect(codeFromGrpcStatus(undefined)).toBe("unknown");
    expect(codeFromGrpcStatus(17)).toBe("unknown");
  });
});

describe("rich error details", () => {
  test("round-trips generated details and metadata through the transport-neutral payload", () => {
    const detail = create(GreetResponseSchema, { message: "generated detail" });
    const connect = new ConnectError("not ready", ConnectCode.FailedPrecondition, { "x-error-meta": "present" }, [
      { desc: GreetResponseSchema, value: detail },
    ]);

    const invariant = asInvariantError(connect);
    expect(invariant).toMatchObject({
      code: "failed_precondition",
      message: "not ready",
    });
    expect(invariant.metadata.get("x-error-meta")).toBe("present");
    expect(invariant.details).toEqual([
      {
        type: GreetResponseSchema.typeName,
        value: Buffer.from(toBinary(GreetResponseSchema, detail)).toString("base64"),
      },
    ]);

    const restored = toConnectError(invariant);
    expect(restored.code).toBe(ConnectCode.FailedPrecondition);
    expect(restored.metadata.get("x-error-meta")).toBe("present");
    const restoredDetail = restored.details[0];
    if (restoredDetail === undefined || !("type" in restoredDetail)) {
      throw new Error("expected a transport-neutral rich detail");
    }
    expect(restoredDetail.type).toBe(GreetResponseSchema.typeName);
    expect(fromBinary(GreetResponseSchema, restoredDetail.value)).toEqual(detail);
  });

  test("preserves both supported detail shapes and ignores malformed payload entries", () => {
    const generated = create(GreetResponseSchema, { message: "typed detail" });
    const connect = toConnectError(
      new InvariantError("internal", "mixed details", [
        { desc: GreetResponseSchema, value: generated },
        { type: "example.test.RawDetail", value: Buffer.from([1, 2, 3]).toString("base64"), debug: "source" },
        null,
        [],
        { type: 7, value: "invalid" },
      ]),
    );

    expect(connect.details).toHaveLength(2);
    const typedDetail = connect.details[0];
    if (typedDetail === undefined || !("desc" in typedDetail)) {
      throw new Error("expected a generated rich detail");
    }
    expect(typedDetail.desc).toBe(GreetResponseSchema);
    expect(typedDetail.value).toEqual(generated);

    const rawDetail = connect.details[1];
    if (rawDetail === undefined || !("type" in rawDetail)) {
      throw new Error("expected a raw rich detail");
    }
    expect(rawDetail).toMatchObject({
      type: "example.test.RawDetail",
      value: Buffer.from([1, 2, 3]),
      debug: "source",
    });

    const rawConnect = new ConnectError("raw", ConnectCode.Internal);
    rawConnect.details = [{ type: "example.test.RawDetail", value: Buffer.from([4, 5, 6]) }];
    expect(asInvariantError(rawConnect).details).toEqual([
      {
        type: "example.test.RawDetail",
        value: Buffer.from([4, 5, 6]).toString("base64"),
      },
    ]);
  });
});
