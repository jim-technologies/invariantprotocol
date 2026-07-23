import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { create, toBinary } from "@bufbuild/protobuf";
import { Code, ConnectError, type ServiceImpl } from "@connectrpc/connect";
import { afterEach, describe, expect, test } from "vitest";

import { runCli, Server } from "../src/index.js";
import {
  type GreetGroupRequest,
  type GreetRequest,
  GreetRequestSchema,
  GreetService,
  type StreamGreetRequest,
} from "./gen/greet_pb.js";

const descriptorPath = resolve(fileURLToPath(new URL("../../python/tests/proto/descriptor.binpb", import.meta.url)));
const temporaryDirectories: string[] = [];

class GreetServicer implements ServiceImpl<typeof GreetService> {
  greet(request: GreetRequest) {
    if (request.name === "status") {
      throw new ConnectError("cli status", Code.FailedPrecondition);
    }
    return { message: `Hi ${request.name}`, mood: 0, tags: request.tags };
  }

  greetGroup(request: GreetGroupRequest) {
    return { messages: request.people.map((person) => `Hi ${person.name}`), count: request.people.length };
  }

  streamGreet(request: StreamGreetRequest) {
    return (async function* () {
      for (let index = 0; index < (request.count || 1); index += 1) {
        yield { message: `Hi ${request.name} #${index}`, mood: 0, tags: {} };
      }
    })();
  }
}

function registeredServer(): Server {
  const server = Server.fromDescriptor(descriptorPath);
  server.register(GreetService, new GreetServicer());
  return server;
}

function requestFile(extension: string, contents: Uint8Array | string): string {
  const directory = mkdtempSync(join(tmpdir(), "invariant-cli-"));
  temporaryDirectories.push(directory);
  const path = join(directory, `request${extension}`);
  writeFileSync(path, contents);
  return path;
}

function requestDirectory(): string {
  const directory = mkdtempSync(join(tmpdir(), "invariant-cli-directory-"));
  temporaryDirectories.push(directory);
  const path = join(directory, "request.json");
  mkdirSync(path);
  return path;
}

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

describe("CLI projection", () => {
  test("loads JSON, .binpb, and .pb request files with canonical decoding", async () => {
    const jsonOutput = await runCli(registeredServer(), [
      "greet.v1.GreetService",
      "Greet",
      "-r",
      requestFile(".json", '{"name":"JsonFile"}'),
    ]);
    expect(JSON.parse(jsonOutput)).toMatchObject({ message: "Hi JsonFile" });

    const encoded = Buffer.concat([
      Buffer.from(toBinary(GreetRequestSchema, create(GreetRequestSchema, { name: "BinaryFile" }))),
      Buffer.from([0x9a, 0x06, 0x03, 0x6e, 0x65, 0x77]),
    ]);

    for (const extension of [".binpb", ".pb"]) {
      const output = await runCli(registeredServer(), [
        "greet.v1.GreetService",
        "Greet",
        "-r",
        requestFile(extension, encoded),
      ]);
      expect(JSON.parse(output)).toMatchObject({ message: "Hi BinaryFile" });
    }
  });

  test("rejects malformed and unsupported request files as invalid_argument", async () => {
    const server = registeredServer();
    await expect(runCli(server, ["GreetService", "Greet"])).rejects.toThrow(/Unknown service\/method/);
    for (const request of [
      '{"name":"Ada","extra":"x"}',
      requestFile(".json", "{"),
      requestFile(".binpb", new Uint8Array([0xff])),
      requestFile(".yaml", "name: Ada"),
      requestDirectory(),
    ]) {
      await expect(runCli(server, ["greet.v1.GreetService", "Greet", "-r", request])).rejects.toMatchObject({
        code: "invalid_argument",
      });
    }
  });

  test("returns unary JSON, streaming NDJSON, and the handler status unchanged", async () => {
    const server = registeredServer();
    const streamInterceptors: string[] = [];
    server.use((next) => async (request) => {
      if (request.stream) {
        streamInterceptors.push(request.url);
      }
      return next(request);
    });
    expect(
      JSON.parse(await runCli(server, ["greet.v1.GreetService", "Greet", "-r", '{"name":"Unary"}'])),
    ).toMatchObject({
      message: "Hi Unary",
    });

    const stream = await runCli(server, ["greet.v1.GreetService", "StreamGreet", "-r", '{"name":"Stream","count":2}']);
    expect(stream.split("\n").map((line) => JSON.parse(line).message)).toEqual(["Hi Stream #0", "Hi Stream #1"]);
    expect(streamInterceptors).toEqual(["invariant-cli:///greet.v1.GreetService/StreamGreet"]);

    await expect(runCli(server, ["greet.v1.GreetService", "Greet", "-r", '{"name":"status"}'])).rejects.toMatchObject({
      code: Code.FailedPrecondition,
      rawMessage: "cli status",
    });
  });

  test("freezes registration even when only rendering help", async () => {
    const server = registeredServer();
    expect(await runCli(server, ["--help"])).toContain("greet.v1.GreetService Greet");
    expect(() => server.use((next) => next)).toThrow(/cannot be changed after execution begins/);
  });
});
