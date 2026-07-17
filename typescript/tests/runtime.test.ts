import { readFileSync } from "node:fs";
import { createServer, request as httpRequest } from "node:http";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { create, createFileRegistry, fromBinary, getExtension, setExtension, toBinary } from "@bufbuild/protobuf";
import {
  DescriptorProtoSchema,
  FieldDescriptorProto_Label,
  FieldDescriptorProto_Type,
  FieldDescriptorProtoSchema,
  FileDescriptorProtoSchema,
  FileDescriptorSetSchema,
  MethodDescriptorProtoSchema,
  ServiceDescriptorProtoSchema,
} from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type ServiceImpl } from "@connectrpc/connect";
import * as grpc from "@grpc/grpc-js";
import { ServerDuplexStreamImpl, ServerWritableStreamImpl } from "@grpc/grpc-js/build/src/server-call.js";
import * as protoLoader from "@grpc/proto-loader";
import { afterEach, describe, expect, test } from "vitest";
import { codeFromGrpcStatus, codeFromHttpStatus, grpcStatusFor, InvariantError } from "../src/errors.js";
import { grpcServiceDefinitionForService } from "../src/grpc.js";
import { CONNECT_STREAM_JSON, httpHandler, ParsedDescriptor, runCli, SchemaGenerator, Server } from "../src/index.js";
import { mcpDispatch } from "../src/mcp.js";
import { http as googleApiHttp } from "./gen/google/api/annotations_pb.js";
import {
  type GreetGroupRequest,
  type GreetRequest,
  GreetResponseSchema,
  GreetService,
  type StreamGreetRequest,
} from "./gen/greet_pb.js";

const here = dirname(fileURLToPath(import.meta.url));
const descriptorPath = resolve(here, "../../python/tests/proto/descriptor.binpb");

class GreetServicer implements ServiceImpl<typeof GreetService> {
  greet(request: GreetRequest) {
    return Promise.resolve({
      message: `Hi ${request.name}`,
      mood: request.mood ?? 0,
      tags: request.tags,
    });
  }

  greetGroup(request: GreetGroupRequest) {
    return Promise.resolve({
      messages: request.people.map((person) => `Hi ${person.name}`),
      count: request.people.length,
    });
  }

  streamGreet(request: StreamGreetRequest) {
    return (async function* () {
      for (let i = 0; i < (request.count || 1); i += 1) {
        yield {
          message: `Hi ${request.name} #${i + 1}`,
          mood: 0,
          tags: {},
        };
      }
    })();
  }
}

function registeredServer(): Server {
  const server = Server.fromDescriptor(descriptorPath);
  server.register(GreetService, new GreetServicer());
  return server;
}

describe("descriptor parsing", () => {
  test("extracts services, methods, messages, enums, and source comments", () => {
    const parsed = ParsedDescriptor.fromFile(descriptorPath);
    const service = parsed.services.get("greet.v1.GreetService");
    expect(service?.name).toBe("GreetService");
    expect(service?.comment).toContain("simple greeting service");
    expect(service?.methods.get("Greet")?.comment).toContain("Greet a person by name");
    expect(parsed.commentForField("greet.v1.GreetRequest", "name")).toContain("Name of the person");
    expect(parsed.enums.get("greet.v1.Mood")?.values.map((value) => value.name)).toEqual([
      "MOOD_UNSPECIFIED",
      "MOOD_HAPPY",
      "MOOD_SAD",
    ]);
  });
});

describe("schema generation", () => {
  test("matches the existing JSON Schema shape", () => {
    const schema = new SchemaGenerator(ParsedDescriptor.fromFile(descriptorPath)).messageToSchema(
      "greet.v1.GreetRequest",
    );
    const props = schema.properties as Record<string, any>;

    expect(schema.type).toBe("object");
    expect(schema.additionalProperties).toBe(false);
    expect(schema.required).toContain("name");
    expect(schema.required).not.toContain("mood");
    expect(schema.required).not.toContain("tags");
    expect(props.name.type).toBe("string");
    expect(props.mood.enum).toEqual(["MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"]);
    expect(props.tags.additionalProperties).toEqual({ type: "string" });
    expect(props.name.description).toContain("Name of the person");

    const group = new SchemaGenerator(ParsedDescriptor.fromFile(descriptorPath)).messageToSchema(
      "greet.v1.GreetGroupRequest",
    );
    const people = (group.properties as Record<string, any>).people;
    expect(people.type).toBe("array");
    expect(people.items.properties.name.type).toBe("string");

    const recursive = new SchemaGenerator(ParsedDescriptor.fromFile(descriptorPath)).messageToSchema(
      "data.v1.RecursiveRecord",
    );
    const parent = (recursive.properties as Record<string, any>).parent;
    expect(parent.properties.parent).toMatchObject({ type: "object" });
    expect(parent.properties.parent.properties).toBeUndefined();
  });
});

describe("server dispatch", () => {
  test("uses Connect's canonical canceled spelling", () => {
    expect(new InvariantError("canceled", "stopped").toPayload().code).toBe("canceled");
    expect(codeFromGrpcStatus(1)).toBe("canceled");
    expect(grpcStatusFor("canceled")).toBe(1);
  });

  test("uses Connect's HTTP status fallback mapping", () => {
    expect(
      [400, 401, 403, 404, 429, 502, 503, 504, 418, 409, 499, 500, 501].map((status) => codeFromHttpStatus(status)),
    ).toEqual([
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
    ]);
  });

  test("registers a generated service with Promise and AsyncIterable handlers", async () => {
    const server = registeredServer();
    expect([...server.tools.keys()].sort()).toEqual([
      "GreetService.Greet",
      "GreetService.GreetGroup",
      "GreetService.StreamGreet",
    ]);

    const response = await server.invoke("GreetService.Greet", {
      name: "Ada",
      mood: "MOOD_HAPPY",
      tags: { team: "eng" },
    });
    expect(server.toJson(server.tools.get("GreetService.Greet")!, response)).toEqual({
      message: "Hi Ada",
      mood: "MOOD_HAPPY",
      tags: { team: "eng" },
    });

    const chunks = [];
    for await (const chunk of server.invokeStream("GreetService.StreamGreet", { name: "Ada", count: 2 })) {
      chunks.push(server.toJson(server.tools.get("GreetService.StreamGreet")!, chunk));
    }
    expect(chunks.map((chunk: any) => chunk.message)).toEqual(["Hi Ada #1", "Hi Ada #2"]);
  });

  test("runs unary interceptors in registration order", async () => {
    const server = registeredServer();
    const calls: string[] = [];
    server.use((next) => async (request) => {
      expect(request.stream).toBe(false);
      if (!request.stream) {
        expect(request.message.$typeName).toBe("greet.v1.GreetRequest");
      }
      calls.push(`before:/${request.service.typeName}/${request.method.name}`);
      const response = await next(request);
      if (!response.stream) {
        expect(response.message.$typeName).toBe("greet.v1.GreetResponse");
      }
      calls.push("after");
      return response;
    });

    await server.invoke("GreetService.Greet", { name: "Ada" });
    expect(calls).toEqual(["before:/greet.v1.GreetService/Greet", "after"]);
  });

  test("accepts stream interceptors that return an AsyncIterable", async () => {
    const server = registeredServer();
    const calls: string[] = [];
    server.use((next) => async (request) => {
      calls.push(`before:/${request.service.typeName}/${request.method.name}`);
      const response = await next(request);
      if (!response.stream) {
        return response;
      }
      const messages = response.message;
      return {
        ...response,
        message: (async function* () {
          for await (const message of messages) {
            expect(message.$typeName).toBe("greet.v1.GreetResponse");
            yield message;
          }
          calls.push("after");
        })(),
      };
    });

    for await (const _chunk of server.invokeStream("GreetService.StreamGreet", { name: "Ada", count: 1 })) {
      // Consume the stream so the interceptor terminal completes.
    }
    expect(calls).toEqual(["before:/greet.v1.GreetService/StreamGreet", "after"]);
  });

  test("rejects duplicate registration", () => {
    const server = registeredServer();
    expect(() => server.register(GreetService, new GreetServicer())).toThrow(/already registered/);
    expect(() => (server.tools as Map<string, unknown>).clear()).toThrow(/read-only/);
  });

  test("rejects generated message descriptors that drift from the descriptor image", () => {
    const fds = fromBinary(FileDescriptorSetSchema, readFileSync(descriptorPath));
    const greetFile = fds.file.find((file) => file.name === "greet.proto");
    const request = greetFile?.messageType.find((message) => message.name === "GreetRequest");
    const firstField = request?.field[0];
    if (firstField === undefined) {
      throw new Error("missing greet request descriptor");
    }
    firstField.name = "drifted_name";
    const registry = createFileRegistry(fds);
    const driftedService = [...registry].find(
      (desc) => desc.kind === "service" && desc.typeName === GreetService.typeName,
    );
    if (driftedService?.kind !== "service") {
      throw new Error("missing drifted service");
    }

    const server = Server.fromDescriptor(descriptorPath);
    expect(() => server.register(driftedService, {})).toThrow(/Generated message .* does not match descriptor\.binpb/);
  });

  test("rejects generated services compiled with different protobuf file semantics", () => {
    const descriptor = (syntax: "proto2" | "proto3") =>
      create(FileDescriptorSetSchema, {
        file: [
          create(FileDescriptorProtoSchema, {
            name: "presence.proto",
            package: "presence.v1",
            syntax,
            messageType: [
              create(DescriptorProtoSchema, {
                name: "Envelope",
                field: [
                  create(FieldDescriptorProtoSchema, {
                    name: "value",
                    number: 1,
                    label: FieldDescriptorProto_Label.OPTIONAL,
                    type: FieldDescriptorProto_Type.STRING,
                  }),
                ],
              }),
            ],
            service: [
              create(ServiceDescriptorProtoSchema, {
                name: "PresenceService",
                method: [
                  create(MethodDescriptorProtoSchema, {
                    name: "Echo",
                    inputType: ".presence.v1.Envelope",
                    outputType: ".presence.v1.Envelope",
                  }),
                ],
              }),
            ],
          }),
        ],
      });

    const generatedService = [...createFileRegistry(descriptor("proto2"))].find(
      (desc) => desc.kind === "service" && desc.typeName === "presence.v1.PresenceService",
    );
    if (generatedService?.kind !== "service") {
      throw new Error("missing generated presence service");
    }
    const server = Server.fromBytes(toBinary(FileDescriptorSetSchema, descriptor("proto3")));
    expect(() => server.register(generatedService, {})).toThrow(
      /Generated message 'presence\.v1\.Envelope' does not match descriptor\.binpb/,
    );
  });

  test("freezes registration, interceptors, filters, and limits on first execution", async () => {
    const server = registeredServer();
    await server.invoke("GreetService.Greet", { name: "Ada" });

    expect(() => server.register(GreetService, new GreetServicer())).toThrow(
      /cannot be changed after execution begins/,
    );
    expect(() => server.use((next) => next)).toThrow(/cannot be changed after execution begins/);
    expect(() => server.include("*")).toThrow(/cannot be changed after execution begins/);
    expect(() => server.setMaxUnaryRequestBytes(1024)).toThrow(/cannot be changed after execution begins/);
    expect(() => server.configureMethod("/greet.v1.GreetService/Greet", {})).toThrow(
      /cannot be changed after execution begins/,
    );

    const grpcBuilt = registeredServer();
    grpcBuilt.grpcServer();
    expect(() => grpcBuilt.exclude("*")).toThrow(/cannot be changed after execution begins/);
    expect(() => grpcBuilt.grpcServer()).toThrow(/already been created/);
    grpcBuilt.forceStop();

    const httpBuilt = registeredServer();
    httpHandler(httpBuilt);
    expect(() => httpBuilt.use((next) => next)).toThrow(/cannot be changed after execution begins/);
  });

  test("requires projection filters before service registration", () => {
    const server = registeredServer();
    expect(() => server.include("*")).toThrow(/before service registration/);
    expect(() => server.exclude("*")).toThrow(/before service registration/);
  });

  test("resets byte limits with zero and rejects invalid values", () => {
    const server = registeredServer();
    server.setMaxUnaryRequestBytes(1);
    server.setMaxUnaryRequestBytes(0);
    expect(server.maxUnaryRequestBytes()).toBe(16 * 1024 * 1024);

    server.configureMethod("/greet.v1.GreetService/Greet", { maxUnaryResponseBytes: 0 });
    expect(server.maxUnaryResponseBytes(server.tools.get("GreetService.Greet"))).toBe(16 * 1024 * 1024);

    expect(() => server.setMaxUnaryResponseBytes(-1)).toThrow(/positive integer/);
    expect(() => server.setMaxStreamRequestBytes(Number.NaN)).toThrow(/positive integer/);
    expect(() => server.setMaxStreamResponseBytes(1.5)).toThrow(/positive integer/);
    expect(() => server.configureMethod("/greet.v1.GreetService/Greet", { maxUnaryResponseBytes: -1 })).toThrow(
      /positive integer/,
    );
  });

  test("serves native client-streaming and bidi methods through the shared Connect interceptor", async () => {
    const fds = create(FileDescriptorSetSchema, {
      file: [
        create(FileDescriptorProtoSchema, {
          name: "streams.proto",
          package: "streams.v1",
          syntax: "proto3",
          messageType: [create(DescriptorProtoSchema, { name: "Envelope" })],
          service: [
            create(ServiceDescriptorProtoSchema, {
              name: "Streams",
              method: [
                create(MethodDescriptorProtoSchema, {
                  name: "Upload",
                  inputType: ".streams.v1.Envelope",
                  outputType: ".streams.v1.Envelope",
                  clientStreaming: true,
                }),
                create(MethodDescriptorProtoSchema, {
                  name: "Chat",
                  inputType: ".streams.v1.Envelope",
                  outputType: ".streams.v1.Envelope",
                  clientStreaming: true,
                  serverStreaming: true,
                }),
              ],
            }),
          ],
        }),
      ],
    });
    const server = Server.fromBytes(toBinary(FileDescriptorSetSchema, fds));
    server.exclude("*");
    const service = server.parsed.services.get("streams.v1.Streams")?.desc;
    if (!service) {
      throw new Error("missing synthetic streaming service");
    }
    const intercepted: string[] = [];
    const inputTypes: string[] = [];
    const outputTypes: string[] = [];
    server.use((next) => async (request) => {
      expect(request.stream).toBe(true);
      intercepted.push(`/${request.service.typeName}/${request.method.name}`);
      if (!request.stream) {
        return next(request);
      }
      const inputs = request.message;
      const response = await next({
        ...request,
        message: (async function* () {
          for await (const message of inputs) {
            inputTypes.push(message.$typeName);
            yield message;
          }
        })(),
      });
      if (!response.stream) {
        return response;
      }
      const outputs = response.message;
      return {
        ...response,
        message: (async function* () {
          for await (const message of outputs) {
            outputTypes.push(message.$typeName);
            yield message;
          }
        })(),
      };
    });
    server.register(service, {
      async upload(requests: AsyncIterable<unknown>) {
        for await (const _request of requests) {
          // Consume the client stream before producing its one response.
        }
        return {};
      },
      chat(requests: AsyncIterable<unknown>) {
        return (async function* () {
          const first = await requests[Symbol.asyncIterator]().next();
          if (!first.done) {
            yield first.value;
          }
        })();
      },
    } as never);

    expect(server.tools.size).toBe(0);
    const definition = grpcServiceDefinitionForService(service);
    expect(definition.upload).toMatchObject({ requestStream: true, responseStream: false });
    expect(definition.chat).toMatchObject({ requestStream: true, responseStream: true });

    const started = await startGrpc(server);
    const Client = grpc.makeGenericClientConstructor(definition, service.typeName);
    const client = new Client(started.address, grpc.credentials.createInsecure());
    try {
      await new Promise<void>((resolveCall, rejectCall) => {
        const call = (client as any).upload((error: grpc.ServiceError | null) => {
          if (error) {
            rejectCall(error);
          } else {
            resolveCall();
          }
        }) as grpc.ClientWritableStream<unknown>;
        call.write({});
        call.write({});
        call.end();
      });

      const bidiMessages = await new Promise<unknown[]>((resolveCall, rejectCall) => {
        const messages: unknown[] = [];
        const call = (client as any).chat() as grpc.ClientDuplexStream<unknown, unknown>;
        call.on("data", (message) => messages.push(message));
        call.on("error", rejectCall);
        call.on("end", () => resolveCall(messages));
        call.write({});
        call.end();
      });
      expect(bidiMessages).toHaveLength(1);
    } finally {
      client.close();
      started.server.forceShutdown();
    }

    expect(intercepted).toEqual(["/streams.v1.Streams/Upload", "/streams.v1.Streams/Chat"]);
    expect(inputTypes).toEqual(Array(3).fill("streams.v1.Envelope"));
    expect(outputTypes).toEqual(Array(2).fill("streams.v1.Envelope"));
  });

  test("applies native gRPC backpressure to server-streaming and bidi responses", async () => {
    const fds = create(FileDescriptorSetSchema, {
      file: [
        create(FileDescriptorProtoSchema, {
          name: "backpressure.proto",
          package: "backpressure.v1",
          syntax: "proto3",
          messageType: [
            create(DescriptorProtoSchema, {
              name: "Envelope",
              field: [
                create(FieldDescriptorProtoSchema, {
                  name: "payload",
                  number: 1,
                  label: FieldDescriptorProto_Label.OPTIONAL,
                  type: FieldDescriptorProto_Type.BYTES,
                }),
              ],
            }),
          ],
          service: [
            create(ServiceDescriptorProtoSchema, {
              name: "Streams",
              method: [
                create(MethodDescriptorProtoSchema, {
                  name: "Fanout",
                  inputType: ".backpressure.v1.Envelope",
                  outputType: ".backpressure.v1.Envelope",
                  serverStreaming: true,
                }),
                create(MethodDescriptorProtoSchema, {
                  name: "Chat",
                  inputType: ".backpressure.v1.Envelope",
                  outputType: ".backpressure.v1.Envelope",
                  clientStreaming: true,
                  serverStreaming: true,
                }),
              ],
            }),
          ],
        }),
      ],
    });
    const server = Server.fromBytes(toBinary(FileDescriptorSetSchema, fds));
    server.exclude("*");
    const service = server.parsed.services.get("backpressure.v1.Streams")?.desc;
    if (!service) {
      throw new Error("missing synthetic backpressure service");
    }

    const totalMessages = 4;
    const payload = new Uint8Array([1, 2, 3]);
    const produced = { fanout: 0, chat: 0 };
    let finishFanout!: () => void;
    let finishChat!: () => void;
    const fanoutFinished = new Promise<void>((resolveFinished) => {
      finishFanout = resolveFinished;
    });
    const chatFinished = new Promise<void>((resolveFinished) => {
      finishChat = resolveFinished;
    });
    const responses = (kind: keyof typeof produced) =>
      (async function* () {
        try {
          for (let i = 0; i < totalMessages; i += 1) {
            produced[kind] += 1;
            yield { payload };
          }
        } finally {
          if (kind === "fanout") {
            finishFanout();
          } else {
            finishChat();
          }
        }
      })();

    server.register(service, {
      fanout() {
        return responses("fanout");
      },
      chat(requests: AsyncIterable<unknown>) {
        return (async function* () {
          const first = await requests[Symbol.asyncIterator]().next();
          if (!first.done) {
            yield* responses("chat");
          }
        })();
      },
    } as never);

    const definition = grpcServiceDefinitionForService(service);
    const started = await startGrpc(server);
    const Client = grpc.makeGenericClientConstructor(definition, service.typeName);
    const client = new Client(started.address, grpc.credentials.createInsecure());
    const waitForProgress = async (kind: keyof typeof produced) => {
      const deadline = Date.now() + 5_000;
      while (produced[kind] === 0 && Date.now() < deadline) {
        await new Promise((resolveDelay) => setTimeout(resolveDelay, 10));
      }
      expect(produced[kind]).toBe(1);
    };
    const waitForFinished = (kind: string, finished: Promise<void>) =>
      new Promise<void>((resolveFinished, rejectFinished) => {
        const timer = setTimeout(
          () => rejectFinished(new Error(`${kind} handler did not close after cancellation`)),
          5_000,
        );
        finished.then(
          () => {
            clearTimeout(timer);
            resolveFinished();
          },
          (error) => {
            clearTimeout(timer);
            rejectFinished(error);
          },
        );
      });
    type WritablePrototype = {
      write(...args: any[]): boolean;
    };
    const patchFirstWrite = (
      prototype: WritablePrototype,
      blocked: (call: { emit(event: string): boolean }) => void,
    ) => {
      const original = prototype.write;
      let first = true;
      prototype.write = function (this: { emit(event: string): boolean }, ...args: any[]): boolean {
        const accepted = original.apply(this, args);
        if (first) {
          first = false;
          blocked(this);
          return false;
        }
        return accepted;
      };
      return () => {
        prototype.write = original;
      };
    };

    let blockedDuplexCall: { emit(event: string): boolean } | undefined;
    const restoreWritable = patchFirstWrite(
      ServerWritableStreamImpl.prototype as unknown as WritablePrototype,
      () => undefined,
    );
    const restoreDuplex = patchFirstWrite(ServerDuplexStreamImpl.prototype as unknown as WritablePrototype, (call) => {
      blockedDuplexCall = call;
    });

    try {
      const fanout = (client as any).fanout({
        payload: new Uint8Array(),
      }) as grpc.ClientReadableStream<unknown>;
      fanout.on("data", () => undefined);
      fanout.on("error", () => {
        // Cancellation is expected after proving the blocked write is awaited.
      });
      await waitForProgress("fanout");
      await new Promise((resolveTurn) => setTimeout(resolveTurn, 25));
      expect(produced.fanout).toBe(1);
      fanout.cancel();
      await waitForFinished("fanout", fanoutFinished);

      const chatMessages = new Promise<unknown[]>((resolveMessages, rejectMessages) => {
        const messages: unknown[] = [];
        const chat = (client as any).chat() as grpc.ClientDuplexStream<unknown, unknown>;
        chat.on("data", (message) => messages.push(message));
        chat.on("error", rejectMessages);
        chat.on("end", () => resolveMessages(messages));
        chat.write({ payload: new Uint8Array([1]) });
        chat.end();
      });
      await waitForProgress("chat");
      await new Promise((resolveTurn) => setTimeout(resolveTurn, 25));
      expect(produced.chat).toBe(1);
      if (!blockedDuplexCall) {
        throw new Error("bidi response did not report backpressure");
      }
      blockedDuplexCall.emit("drain");
      expect(await chatMessages).toHaveLength(totalMessages);
      await waitForFinished("chat", chatFinished);
    } finally {
      restoreDuplex();
      restoreWritable();
      client.close();
      started.server.forceShutdown();
    }
  });
});

describe("projections", () => {
  const servers: Array<ReturnType<typeof createServer>> = [];
  const grpcServers: grpc.Server[] = [];

  afterEach(async () => {
    await Promise.all(
      servers.map(
        (server) =>
          new Promise<void>((resolveClose) => {
            server.close(() => resolveClose());
          }),
      ),
    );
    servers.length = 0;
    for (const server of grpcServers.splice(0)) {
      server.forceShutdown();
    }
  });

  test("runs CLI and MCP calls", async () => {
    const server = registeredServer();

    const cli = JSON.parse(await runCli(server, ["GreetService", "Greet", "-r", '{"name":"Ada"}']));
    expect(cli).toEqual({ message: "Hi Ada" });
    await expect(runCli(server, ["GreetService", "Greet", "extra"])).rejects.toThrow("Unknown argument: extra");
    await expect(runCli(server, ["GreetService", "Greet", "-r", "{}", "extra"])).rejects.toThrow(
      "Unexpected argument: extra",
    );

    const mcp = await mcpDispatch(server, {
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "GreetService.Greet", arguments: { name: "Ada" } },
    });
    if (mcp === undefined) {
      throw new Error("MCP tool call returned no response");
    }
    expect((mcp.result as any).content[0].text).toContain("Hi Ada");
  });

  test("serves Connect-style HTTP JSON and descriptor endpoints", async () => {
    const server = registeredServer();
    const nodeServer = createServer((req, res) => {
      void httpHandler(server)(req, res);
    });
    servers.push(nodeServer);
    await new Promise<void>((resolveListen) => nodeServer.listen(0, "127.0.0.1", resolveListen));
    const address = nodeServer.address();
    if (!address || typeof address === "string") {
      throw new Error("missing test server address");
    }
    const base = `http://127.0.0.1:${address.port}`;

    const catalog = await fetch(`${base}/__invariant/tools`).then((res) => res.json());
    expect(catalog.tools.map((tool: any) => tool.name)).toContain("GreetService.Greet");

    const response = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Ada", mood: "MOOD_HAPPY" }),
    });
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ message: "Hi Ada", mood: "MOOD_HAPPY" });

    const descriptor = await fetch(`${base}/__invariant/descriptor.binpb`);
    expect(descriptor.status).toBe(200);
    expect(descriptor.headers.get("content-type")).toBe("application/proto");
    expect((await descriptor.arrayBuffer()).byteLength).toBeGreaterThan(0);

    const grpcWeb = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/grpc-web+proto" },
      body: Buffer.alloc(5),
    });
    expect(grpcWeb.status).not.toBe(200);
  });

  test("returns a listening caller-owned HTTP server from the public host wrapper", async () => {
    const invariant = registeredServer();
    const nodeServer = await invariant.serveHttp(0, "127.0.0.1");
    servers.push(nodeServer);

    expect(nodeServer.listening).toBe(true);
    const address = nodeServer.address();
    if (!address || typeof address === "string") {
      throw new Error("missing test server address");
    }
    const health = await fetch(`http://127.0.0.1:${address.port}/healthz`);
    expect(health.status).toBe(200);
    expect(await health.json()).toEqual({ status: "ok" });
    expect(() => invariant.setMaxUnaryRequestBytes(1024)).toThrow(/cannot be changed after execution begins/);
  });

  test("serves Connect streaming envelopes over HTTP", async () => {
    const server = registeredServer();
    const nodeServer = createServer((req, res) => {
      void httpHandler(server)(req, res);
    });
    servers.push(nodeServer);
    await new Promise<void>((resolveListen) => nodeServer.listen(0, "127.0.0.1", resolveListen));
    const address = nodeServer.address();
    if (!address || typeof address === "string") {
      throw new Error("missing test server address");
    }
    const base = `http://127.0.0.1:${address.port}`;
    const body = connectEnvelope(0, Buffer.from(JSON.stringify({ name: "Ada", count: 2 })));

    const response = await fetch(`${base}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: { "content-type": CONNECT_STREAM_JSON },
      body,
    });
    expect(response.status).toBe(200);
    const bytes = new Uint8Array(await response.arrayBuffer());
    const frames = readConnectFrames(bytes);
    expect(frames.map((frame) => frame.flags)).toEqual([0, 0, 2]);
    expect(JSON.parse(Buffer.from(frames[0]!.payload).toString("utf8")).message).toBe("Hi Ada #1");
  });

  test("serves unary protobuf over HTTP", async () => {
    const server = registeredServer();
    const nodeServer = createServer((req, res) => {
      void httpHandler(server)(req, res);
    });
    servers.push(nodeServer);
    await new Promise<void>((resolveListen) => nodeServer.listen(0, "127.0.0.1", resolveListen));
    const address = nodeServer.address();
    if (!address || typeof address === "string") {
      throw new Error("missing test server address");
    }
    const base = `http://127.0.0.1:${address.port}`;
    const tool = server.tools.get("GreetService.Greet")!;
    const request = server.coerceMessage(tool.inputDesc, { name: "Ada" });

    const response = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/proto", accept: "application/proto" },
      body: toBinary(tool.inputDesc, request),
    });
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toBe("application/proto");
    expect((await response.arrayBuffer()).byteLength).toBeGreaterThan(0);
  });

  test("enforces independent unary request and response limits with per-method overrides", async () => {
    const requestLimited = registeredServer();
    requestLimited.setMaxUnaryRequestBytes(32);
    const requestBase = await startHttp(requestLimited, servers);
    const oversizedRequest = await fetch(`${requestBase}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "x".repeat(64) }),
    });
    expect(oversizedRequest.status).toBe(429);
    expect(await oversizedRequest.json()).toMatchObject({ code: "resource_exhausted" });

    const responseLimited = Server.fromDescriptor(descriptorPath);
    responseLimited.setMaxUnaryResponseBytes(32);
    responseLimited.register(GreetService, {
      greet: () => Promise.resolve({ message: "x".repeat(128) }),
    });
    const responseBase = await startHttp(responseLimited, servers);
    const oversizedResponse = await fetch(`${responseBase}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Ada" }),
    });
    expect(oversizedResponse.status).toBe(429);
    expect(await oversizedResponse.json()).toMatchObject({ code: "resource_exhausted" });

    const overridden = Server.fromDescriptor(descriptorPath);
    overridden.setMaxUnaryResponseBytes(32);
    overridden.configureMethod("/greet.v1.GreetService/Greet", { maxUnaryResponseBytes: 1024 });
    overridden.register(GreetService, {
      greet: () => Promise.resolve({ message: "x".repeat(128) }),
    });
    const overrideBase = await startHttp(overridden, servers);
    const allowedResponse = await fetch(`${overrideBase}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Ada" }),
    });
    expect(allowedResponse.status).toBe(200);
    expect((await allowedResponse.json()).message).toHaveLength(128);
  });

  test("bounds rich Connect error responses with the unary response limit", async () => {
    const richErrorServer = (maxResponseBytes: number) => {
      const server = Server.fromDescriptor(descriptorPath);
      server.setMaxUnaryResponseBytes(maxResponseBytes);
      server.register(GreetService, {
        greet() {
          throw new ConnectError("invalid", Code.InvalidArgument, undefined, [
            { desc: GreetResponseSchema, value: { message: "x".repeat(4096) } },
          ]);
        },
      });
      return server;
    };

    const limitedBase = await startHttp(richErrorServer(160), servers);
    const limited = await fetch(`${limitedBase}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Ada" }),
    });
    expect(limited.status).toBe(429);
    const limitedBody = new Uint8Array(await limited.arrayBuffer());
    expect(limitedBody.byteLength).toBeLessThanOrEqual(160);
    expect(JSON.parse(Buffer.from(limitedBody).toString("utf8"))).toMatchObject({
      code: "resource_exhausted",
    });

    const tinyBase = await startHttp(richErrorServer(1), servers);
    const tiny = await fetch(`${tinyBase}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Ada" }),
    });
    expect(tiny.status).toBe(429);
    expect(await tiny.text()).toBe("");
  });

  test("enforces Connect streaming request and response limits per message", async () => {
    const requestLimited = registeredServer();
    requestLimited.setMaxStreamRequestBytes(32);
    const requestBase = await startHttp(requestLimited, servers);
    const oversizedRequest = connectEnvelope(0, Buffer.from(JSON.stringify({ name: "x".repeat(64), count: 1 })));
    const requestResponse = await fetch(`${requestBase}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: { "content-type": CONNECT_STREAM_JSON },
      body: oversizedRequest,
    });
    expect(requestResponse.status).toBe(200);
    const requestFrames = readConnectFrames(new Uint8Array(await requestResponse.arrayBuffer()));
    expect(JSON.parse(Buffer.from(requestFrames.at(-1)!.payload).toString("utf8"))).toMatchObject({
      error: { code: "resource_exhausted" },
    });

    const responseLimited = Server.fromDescriptor(descriptorPath);
    responseLimited.setMaxStreamResponseBytes(64);
    responseLimited.register(GreetService, {
      streamGreet: () =>
        (async function* () {
          yield { message: "x".repeat(128) };
        })(),
    });
    const responseBase = await startHttp(responseLimited, servers);
    const response = await fetch(`${responseBase}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: { "content-type": CONNECT_STREAM_JSON },
      body: connectEnvelope(0, Buffer.from(JSON.stringify({ name: "Ada", count: 1 }))),
    });
    expect(response.status).toBe(200);
    const responseFrames = readConnectFrames(new Uint8Array(await response.arrayBuffer()));
    expect(responseFrames).toHaveLength(1);
    expect(responseFrames[0]!.flags).toBe(2);
    expect(responseFrames[0]!.payload.byteLength).toBeLessThanOrEqual(1024 * 1024);
    expect(JSON.parse(Buffer.from(responseFrames[0]!.payload).toString("utf8"))).toMatchObject({
      error: { code: "resource_exhausted" },
    });
  });

  test("bounds Connect end-error and success control envelopes", async () => {
    const richError = Server.fromDescriptor(descriptorPath);
    richError.setMaxStreamResponseBytes(64);
    richError.register(GreetService, {
      streamGreet: () =>
        // biome-ignore lint/correctness/useYield: This stream intentionally fails before its first message.
        (async function* () {
          throw new ConnectError("x".repeat(4096), Code.InvalidArgument);
        })(),
    });
    const richBase = await startHttp(richError, servers);
    const requestBody = connectEnvelope(0, Buffer.from(JSON.stringify({ name: "Ada", count: 1 })));
    const response = await fetch(`${richBase}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: { "content-type": CONNECT_STREAM_JSON },
      body: requestBody,
    });
    const frames = readConnectFrames(new Uint8Array(await response.arrayBuffer()));
    expect(frames).toHaveLength(1);
    expect(frames[0]!.flags).toBe(2);
    expect(frames[0]!.payload.byteLength).toBeGreaterThan(64);
    expect(JSON.parse(Buffer.from(frames[0]!.payload).toString("utf8"))).toMatchObject({
      error: { code: "invalid_argument", message: "x".repeat(4096) },
    });

    const tiny = Server.fromDescriptor(descriptorPath);
    tiny.setMaxStreamResponseBytes(1);
    tiny.register(GreetService, {
      streamGreet: () => (async function* () {})(),
    });
    const tinyBase = await startHttp(tiny, servers);
    const tinyResponse = await fetch(`${tinyBase}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: { "content-type": CONNECT_STREAM_JSON },
      body: requestBody,
    });
    const tinyFrames = readConnectFrames(new Uint8Array(await tinyResponse.arrayBuffer()));
    expect(tinyFrames).toHaveLength(1);
    expect(tinyFrames[0]).toMatchObject({ flags: 2 });
    expect(JSON.parse(Buffer.from(tinyFrames[0]!.payload).toString("utf8"))).toEqual({});

    const hugeError = Server.fromDescriptor(descriptorPath);
    hugeError.setMaxStreamResponseBytes(1);
    hugeError.register(GreetService, {
      streamGreet: () =>
        // biome-ignore lint/correctness/useYield: This stream intentionally fails before its first message.
        (async function* () {
          throw new ConnectError("x".repeat(2 * 1024 * 1024), Code.InvalidArgument);
        })(),
    });
    const hugeBase = await startHttp(hugeError, servers);
    const hugeResponse = await fetch(`${hugeBase}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: { "content-type": CONNECT_STREAM_JSON },
      body: requestBody,
    });
    const hugeFrames = readConnectFrames(new Uint8Array(await hugeResponse.arrayBuffer()));
    expect(hugeFrames).toHaveLength(1);
    expect(hugeFrames[0]).toMatchObject({ flags: 2 });
    expect(hugeFrames[0]!.payload.byteLength).toBeLessThanOrEqual(1024 * 1024);
    expect(JSON.parse(Buffer.from(hugeFrames[0]!.payload).toString("utf8"))).toEqual({
      error: { code: "resource_exhausted" },
    });
  });

  test("enforces absolute Connect deadlines before handler poll and after completion", async () => {
    const busyWait = (milliseconds: number) => {
      const deadline = Date.now() + milliseconds;
      while (Date.now() < deadline) {
        // Intentionally occupy the event loop to reproduce completion racing
        // the deadline timer.
      }
    };
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      greet: () => {
        busyWait(20);
        return { message: "too late" };
      },
      streamGreet: () =>
        (async function* () {
          busyWait(20);
          yield { message: "too late" };
        })(),
    });
    const base = await startHttp(server, servers);

    const unary = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "connect-timeout-ms": "1",
      },
      body: JSON.stringify({ name: "cpu" }),
    });
    expect(unary.status).toBe(504);
    expect(await unary.json()).toMatchObject({ code: "deadline_exceeded" });

    const stream = await fetch(`${base}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: {
        "content-type": CONNECT_STREAM_JSON,
        "connect-timeout-ms": "1",
      },
      body: connectEnvelope(0, Buffer.from(JSON.stringify({ name: "cpu", count: 1 }))),
    });
    expect(stream.status).toBe(200);
    const streamFrames = readConnectFrames(new Uint8Array(await stream.arrayBuffer()));
    expect(streamFrames).toHaveLength(1);
    expect(JSON.parse(Buffer.from(streamFrames[0]!.payload).toString("utf8"))).toMatchObject({
      error: { code: "deadline_exceeded" },
    });

    const mcp = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: {
        accept: "application/json, text/event-stream",
        "content-type": "application/json",
        "connect-timeout-ms": "1",
        "mcp-protocol-version": "2025-11-25",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "tools/call",
        params: { name: "GreetService.Greet", arguments: { name: "cpu" } },
      }),
    });
    expect(mcp.status).toBe(504);
    expect(await mcp.json()).toMatchObject({ code: "deadline_exceeded" });

    const unaryBody = Buffer.from(JSON.stringify({ name: "delayed" }));
    const delayedUnary = await delayedPost(
      `${base}/greet.v1.GreetService/Greet`,
      {
        "content-type": "application/json",
        "connect-timeout-ms": "10",
      },
      unaryBody,
      30,
    );
    expect(delayedUnary.status).toBe(504);
    expect(JSON.parse(delayedUnary.body.toString("utf8"))).toMatchObject({
      code: "deadline_exceeded",
    });

    const streamBody = Buffer.from(connectEnvelope(0, Buffer.from(JSON.stringify({ name: "delayed", count: 1 }))));
    const delayedStream = await delayedPost(
      `${base}/greet.v1.GreetService/StreamGreet`,
      {
        "content-type": CONNECT_STREAM_JSON,
        "connect-timeout-ms": "10",
      },
      streamBody,
      30,
    );
    expect(delayedStream.status).toBe(200);
    const delayedFrames = readConnectFrames(delayedStream.body);
    expect(delayedFrames).toHaveLength(1);
    expect(JSON.parse(Buffer.from(delayedFrames[0]!.payload).toString("utf8"))).toMatchObject({
      error: { code: "deadline_exceeded" },
    });
  });

  test("preserves long Connect deadlines without overflowing Node timers", async () => {
    const observed: Array<{ protocol: string; timeoutMs: number }> = [];
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      greet: async (request, context) => {
        observed.push({
          protocol: context.protocolName,
          timeoutMs: context.timeoutMs() ?? Number.NaN,
        });
        await new Promise((resolveDelay) => setTimeout(resolveDelay, 20));
        return { message: `Hi ${request.name}` };
      },
      streamGreet: (request, context) =>
        (async function* () {
          observed.push({
            protocol: context.protocolName,
            timeoutMs: context.timeoutMs() ?? Number.NaN,
          });
          await new Promise((resolveDelay) => setTimeout(resolveDelay, 20));
          yield { message: `Hi ${request.name}` };
        })(),
    });
    const base = await startHttp(server, servers);
    const longTimeoutMs = "3000000000";

    const unary = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "connect-timeout-ms": longTimeoutMs,
      },
      body: JSON.stringify({ name: "unary" }),
    });
    expect(unary.status).toBe(200);
    expect(await unary.json()).toMatchObject({ message: "Hi unary" });

    const stream = await fetch(`${base}/greet.v1.GreetService/StreamGreet`, {
      method: "POST",
      headers: {
        "content-type": CONNECT_STREAM_JSON,
        "connect-timeout-ms": longTimeoutMs,
      },
      body: connectEnvelope(0, Buffer.from(JSON.stringify({ name: "stream", count: 1 }))),
    });
    expect(stream.status).toBe(200);
    const streamFrames = readConnectFrames(new Uint8Array(await stream.arrayBuffer()));
    expect(streamFrames.map((frame) => frame.flags)).toEqual([0, 2]);
    expect(JSON.parse(Buffer.from(streamFrames[0]!.payload).toString("utf8"))).toMatchObject({
      message: "Hi stream",
    });

    const mcp = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: {
        accept: "application/json, text/event-stream",
        "content-type": "application/json",
        "connect-timeout-ms": longTimeoutMs,
        "mcp-protocol-version": "2025-11-25",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "tools/call",
        params: { name: "GreetService.Greet", arguments: { name: "mcp" } },
      }),
    });
    expect(mcp.status).toBe(200);
    expect(await mcp.json()).toMatchObject({
      result: {
        content: [{ type: "text", text: expect.stringContaining("Hi mcp") }],
      },
    });

    expect(observed.map(({ protocol }) => protocol)).toEqual(["connect", "connect", "mcp"]);
    for (const { timeoutMs } of observed) {
      expect(timeoutMs).toBeGreaterThan(2_147_483_647);
      expect(timeoutMs).toBeLessThanOrEqual(Number(longTimeoutMs));
    }
  });

  test("rejects malformed Connect timeout headers consistently", async () => {
    const base = await startHttp(registeredServer(), servers);
    for (const invalidTimeout of ["0", "-1", "+1", "1.0", "abc", "12345678901"]) {
      const unary = await fetch(`${base}/greet.v1.GreetService/Greet`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "connect-timeout-ms": invalidTimeout,
        },
        body: JSON.stringify({ name: "Ada" }),
      });
      expect(unary.status, invalidTimeout).toBe(400);
      expect(await unary.json()).toMatchObject({ code: "invalid_argument" });

      const stream = await fetch(`${base}/greet.v1.GreetService/StreamGreet`, {
        method: "POST",
        headers: {
          "content-type": CONNECT_STREAM_JSON,
          "connect-timeout-ms": invalidTimeout,
        },
        body: connectEnvelope(0, Buffer.from(JSON.stringify({ name: "Ada", count: 1 }))),
      });
      expect(stream.status, invalidTimeout).toBe(200);
      const frames = readConnectFrames(new Uint8Array(await stream.arrayBuffer()));
      expect(frames).toHaveLength(1);
      expect(JSON.parse(Buffer.from(frames[0]!.payload).toString("utf8"))).toMatchObject({
        error: { code: "invalid_argument" },
      });
    }
  });

  test("serves dynamic gRPC unary and server-streaming methods", async () => {
    const server = registeredServer();
    const started = await startGrpc(server);
    grpcServers.push(started.server);
    const Client = grpc.makeGenericClientConstructor(
      grpcServiceDefinitionForService(GreetService),
      GreetService.typeName,
    );
    const client = new Client(started.address, grpc.credentials.createInsecure());

    const unary = await unaryCall(client, "greet", { name: "Ada", mood: "MOOD_HAPPY" });
    expect(server.toJson(server.tools.get("GreetService.Greet")!, unary)).toEqual({
      message: "Hi Ada",
      mood: "MOOD_HAPPY",
    });

    const chunks = await streamCall(client, "streamGreet", { name: "Ada", count: 2 });
    expect(chunks.map((chunk: any) => chunk.message)).toEqual(["Hi Ada #1", "Hi Ada #2"]);
    client.close();
  });

  test("keeps filtered methods available on the complete native gRPC service", async () => {
    const server = Server.fromDescriptor(descriptorPath);
    server.exclude("*");
    server.register(GreetService, new GreetServicer());
    expect(server.tools.size).toBe(0);

    const started = await startGrpc(server);
    grpcServers.push(started.server);
    const Client = grpc.makeGenericClientConstructor(
      grpcServiceDefinitionForService(GreetService),
      GreetService.typeName,
    );
    const client = new Client(started.address, grpc.credentials.createInsecure());
    const response = await unaryCall(client, "greet", { name: "Ada" });
    expect(response.message).toBe("Hi Ada");
    client.close();
  });

  test("registers gRPC reflection", async () => {
    const server = registeredServer();
    const started = await startGrpc(server);
    grpcServers.push(started.server);

    const protoPath = resolve(
      here,
      "../../node_modules/@grpc/reflection/build/proto/grpc/reflection/v1/reflection.proto",
    );
    const pkgDef = protoLoader.loadSync(protoPath, { oneofs: true });
    const pkg = grpc.loadPackageDefinition(pkgDef) as any;
    const client = new pkg.grpc.reflection.v1.ServerReflection(started.address, grpc.credentials.createInsecure());
    const stream = client.ServerReflectionInfo();
    const response = await new Promise<any>((resolveResponse, rejectResponse) => {
      stream.on("data", resolveResponse);
      stream.on("error", rejectResponse);
      stream.write({ listServices: "*" });
    });
    stream.end();

    expect(response.listServicesResponse.service.map((svc: any) => svc.name)).toContain("greet.v1.GreetService");
    client.close();
  });

  test("gRPC reflection excludes unregistered services from symbols and returned files", async () => {
    const fds = fromBinary(FileDescriptorSetSchema, readFileSync(descriptorPath));
    fds.file.push(
      create(FileDescriptorProtoSchema, {
        name: "hidden.proto",
        package: "greet.v1",
        syntax: "proto3",
        dependency: ["greet.proto"],
        service: [
          create(ServiceDescriptorProtoSchema, {
            name: "HiddenService",
            method: [
              create(MethodDescriptorProtoSchema, {
                name: "Hidden",
                inputType: ".greet.v1.GreetRequest",
                outputType: ".greet.v1.GreetResponse",
              }),
            ],
          }),
        ],
      }),
    );

    const server = Server.fromBytes(toBinary(FileDescriptorSetSchema, fds));
    server.register(GreetService, new GreetServicer());
    const started = await startGrpc(server);
    grpcServers.push(started.server);

    const protoPath = resolve(
      here,
      "../../node_modules/@grpc/reflection/build/proto/grpc/reflection/v1/reflection.proto",
    );
    const pkgDef = protoLoader.loadSync(protoPath, { oneofs: true });
    const pkg = grpc.loadPackageDefinition(pkgDef) as any;
    const client = new pkg.grpc.reflection.v1.ServerReflection(started.address, grpc.credentials.createInsecure());
    const stream = client.ServerReflectionInfo();

    const hidden = await reflectionCall(stream, {
      fileContainingSymbol: "greet.v1.HiddenService",
    });
    expect(hidden.errorResponse).toMatchObject({ errorCode: grpc.status.NOT_FOUND });

    const registered = await reflectionCall(stream, {
      fileContainingSymbol: "greet.v1.GreetService",
    });
    const files = registered.fileDescriptorResponse.fileDescriptorProto.map((bytes: Uint8Array) =>
      fromBinary(FileDescriptorProtoSchema, bytes),
    );
    const reflectedGreet = files.find((file: { name: string }) => file.name === "greet.proto");
    expect(reflectedGreet?.service.map((service: { name: string }) => service.name)).toEqual(["GreetService"]);

    stream.end();
    client.close();
  });

  test("proxies unary calls to a remote gRPC server", async () => {
    const backend = registeredServer();
    const started = await startGrpc(backend);
    grpcServers.push(started.server);

    const proxy = Server.fromDescriptor(descriptorPath);
    const Client = grpc.makeGenericClientConstructor(
      grpcServiceDefinitionForService(GreetService),
      GreetService.typeName,
    );
    const client = new Client(started.address, grpc.credentials.createInsecure());
    proxy.connectGrpc(GreetService, client);
    const response = await proxy.invoke("GreetService.Greet", { name: "Ada" });
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, response)).toEqual({ message: "Hi Ada" });

    const proxyStarted = await startGrpc(proxy);
    grpcServers.push(proxyStarted.server);
    const ProxyClient = grpc.makeGenericClientConstructor(
      grpcServiceDefinitionForService(GreetService),
      GreetService.typeName,
    );
    const proxyClient = new ProxyClient(proxyStarted.address, grpc.credentials.createInsecure());
    const native = await unaryCall(proxyClient, "greet", { name: "Grace" });
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, native)).toEqual({ message: "Hi Grace" });

    const protoPath = resolve(
      here,
      "../../node_modules/@grpc/reflection/build/proto/grpc/reflection/v1/reflection.proto",
    );
    const pkgDef = protoLoader.loadSync(protoPath, { oneofs: true });
    const pkg = grpc.loadPackageDefinition(pkgDef) as any;
    const reflection = new pkg.grpc.reflection.v1.ServerReflection(
      proxyStarted.address,
      grpc.credentials.createInsecure(),
    );
    const stream = reflection.ServerReflectionInfo();
    const listed = await reflectionCall(stream, { listServices: "*" });
    expect((listed.listServicesResponse.service ?? []).map((svc: { name: string }) => svc.name)).toContain(
      GreetService.typeName,
    );
    const symbol = await reflectionCall(stream, {
      fileContainingSymbol: GreetService.typeName,
    });
    const files = symbol.fileDescriptorResponse.fileDescriptorProto.map((bytes: Uint8Array) =>
      fromBinary(FileDescriptorProtoSchema, bytes),
    );
    const reflectedGreet = files
      .flatMap((file: { service: { name: string; method: { name: string }[] }[] }) => file.service)
      .find((service: { name: string }) => service.name === "GreetService");
    expect(reflectedGreet?.method.map((method: { name: string }) => method.name)).toEqual(["Greet", "GreetGroup"]);
    const hiddenStream = await reflectionCall(stream, {
      fileContainingSymbol: `${GreetService.typeName}.StreamGreet`,
    });
    expect(hiddenStream.errorResponse).toMatchObject({ errorCode: grpc.status.NOT_FOUND });

    stream.end();
    reflection.close();
    proxyClient.close();
    client.close();
  });

  test("proxies HTTP calls with google.api.http annotations, auth, and observer", async () => {
    const observed: any[] = [];
    const remote = createServer((req, res) => {
      const url = new URL(req.url ?? "/", "http://remote");
      if (req.method === "GET" && url.pathname === "/v1/greet/Ada") {
        expect(req.headers["x-test-auth"]).toBe("yes");
        res.setHeader("content-type", "application/json");
        res.end(
          JSON.stringify({ message: `REST ${url.pathname.split("/").at(-1)}`, mood: url.searchParams.get("mood") }),
        );
        return;
      }
      if (req.method === "GET" && url.pathname === "/v1/greet/Bad") {
        res.statusCode = 400;
        res.setHeader("content-type", "application/json");
        res.end(JSON.stringify({ code: "invalid_argument", message: "bad name" }));
        return;
      }
      if (req.method === "GET" && url.pathname === "/v1/greet/Cancel") {
        res.statusCode = 499;
        res.setHeader("content-type", "application/json");
        res.end(JSON.stringify({ code: "canceled", message: "request canceled" }));
        return;
      }
      if (req.method === "GET" && url.pathname === "/v1/greet/ResponseBody") {
        res.setHeader("content-type", "application/json");
        res.end(JSON.stringify("wrapped response"));
        return;
      }
      res.statusCode = 404;
      res.end(JSON.stringify({ code: "not_found", message: "missing" }));
    });
    servers.push(remote);
    await new Promise<void>((resolveListen) => remote.listen(0, "127.0.0.1", resolveListen));
    const address = remote.address();
    if (!address || typeof address === "string") {
      throw new Error("missing test server address");
    }

    const proxy = Server.fromDescriptor(descriptorPath);
    proxy.connectHttp(`http://127.0.0.1:${address.port}`, {
      auth: { headerProvider: () => ({ "x-test-auth": "yes" }) },
      observer: (response) => {
        observed.push(response);
      },
    });
    const response = await proxy.invoke("GreetService.Greet", { name: "Ada", mood: "MOOD_HAPPY" });
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, response)).toEqual({
      message: "REST Ada",
      mood: "MOOD_HAPPY",
    });
    expect(observed).toHaveLength(1);
    expect(observed[0].request.url).toContain("/v1/greet/Ada");

    await expect(proxy.invoke("GreetService.Greet", { name: "Bad" })).rejects.toMatchObject({
      code: "invalid_argument",
      message: "bad name",
    });
    expect(observed).toHaveLength(2);

    await expect(proxy.invoke("GreetService.Greet", { name: "Cancel" })).rejects.toMatchObject({
      code: "canceled",
      message: "request canceled",
    });
    expect(observed).toHaveLength(3);

    const fds = fromBinary(FileDescriptorSetSchema, readFileSync(descriptorPath));
    const greetFile = fds.file.find((file) => file.name === "greet.proto");
    const greetMethod = greetFile?.service
      .find((service) => service.name === "GreetService")
      ?.method.find((method) => method.name === "Greet");
    const responseField = greetFile?.messageType
      .find((message) => message.name === "GreetResponse")
      ?.field.find((field) => field.name === "message");
    if (!greetMethod?.options || !responseField) {
      throw new Error("missing annotated Greet descriptors");
    }
    const rule = getExtension(greetMethod.options, googleApiHttp);
    rule.responseBody = "response_text";
    setExtension(greetMethod.options, googleApiHttp, rule);
    responseField.name = "response_text";
    responseField.jsonName = "wireText";

    const responseBodyProxy = Server.fromBytes(toBinary(FileDescriptorSetSchema, fds));
    responseBodyProxy.connectHttp(`http://127.0.0.1:${address.port}`);
    const wrapped = await responseBodyProxy.invoke("GreetService.Greet", { name: "ResponseBody" });
    expect(responseBodyProxy.toJson(responseBodyProxy.tools.get("GreetService.Greet")!, wrapped)).toEqual({
      response_text: "wrapped response",
    });
  });
});

async function startGrpc(server: Server): Promise<{ server: grpc.Server; address: string }> {
  const grpcServer = server.grpcServer();
  const port = await new Promise<number>((resolvePort, rejectPort) => {
    grpcServer.bindAsync("127.0.0.1:0", grpc.ServerCredentials.createInsecure(), (err, boundPort) => {
      if (err) {
        rejectPort(err);
        return;
      }
      resolvePort(boundPort);
    });
  });
  return { server: grpcServer, address: `127.0.0.1:${port}` };
}

async function startHttp(server: Server, servers: Array<ReturnType<typeof createServer>>): Promise<string> {
  const nodeServer = createServer((req, res) => {
    void httpHandler(server)(req, res);
  });
  servers.push(nodeServer);
  await new Promise<void>((resolveListen) => nodeServer.listen(0, "127.0.0.1", resolveListen));
  const address = nodeServer.address();
  if (!address || typeof address === "string") {
    throw new Error("missing test server address");
  }
  return `http://127.0.0.1:${address.port}`;
}

async function delayedPost(
  url: string,
  headers: Record<string, string>,
  body: Buffer,
  delayMs: number,
): Promise<{ status: number; body: Buffer }> {
  return new Promise((resolveResponse, rejectResponse) => {
    const request = httpRequest(
      url,
      {
        method: "POST",
        headers: {
          ...headers,
          "content-length": String(body.length),
        },
      },
      (response) => {
        const chunks: Buffer[] = [];
        response.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
        response.on("end", () => {
          resolveResponse({
            status: response.statusCode ?? 0,
            body: Buffer.concat(chunks),
          });
        });
      },
    );
    request.on("error", rejectResponse);
    const split = Math.max(1, Math.floor(body.length / 2));
    request.write(body.subarray(0, split));
    setTimeout(() => request.end(body.subarray(split)), delayMs);
  });
}

function unaryCall(client: grpc.Client, method: string, request: unknown): Promise<any> {
  return new Promise((resolveCall, rejectCall) => {
    (client as any)[method](request, (err: grpc.ServiceError | null, response: any) => {
      if (err) {
        rejectCall(err);
        return;
      }
      resolveCall(response);
    });
  });
}

function streamCall(client: grpc.Client, method: string, request: unknown): Promise<any[]> {
  return new Promise((resolveCall, rejectCall) => {
    const out: any[] = [];
    const stream = (client as any)[method](request);
    stream.on("data", (chunk: any) => out.push(chunk));
    stream.on("end", () => resolveCall(out));
    stream.on("error", rejectCall);
  });
}

function reflectionCall(stream: grpc.ClientDuplexStream<unknown, any>, request: unknown): Promise<any> {
  return new Promise((resolveCall, rejectCall) => {
    stream.once("data", resolveCall);
    stream.once("error", rejectCall);
    stream.write(request);
  });
}

function connectEnvelope(flags: number, data: Uint8Array): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(5 + data.length);
  out[0] = flags;
  new DataView(out.buffer).setUint32(1, data.length);
  out.set(data, 5);
  return out;
}

function readConnectFrames(bytes: Uint8Array): Array<{ flags: number; payload: Uint8Array }> {
  const frames = [];
  let offset = 0;
  while (offset < bytes.length) {
    const flags = bytes[offset] ?? 0;
    const len = new DataView(bytes.buffer, bytes.byteOffset + offset + 1, 4).getUint32(0);
    offset += 5;
    frames.push({ flags, payload: bytes.subarray(offset, offset + len) });
    offset += len;
  }
  return frames;
}
