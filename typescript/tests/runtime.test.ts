import { createServer } from "node:http";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";
import { create, createFileRegistry, fromBinary, toBinary } from "@bufbuild/protobuf";
import {
  DescriptorProtoSchema,
  FileDescriptorProtoSchema,
  FileDescriptorSetSchema,
  MethodDescriptorProtoSchema,
  ServiceDescriptorProtoSchema,
} from "@bufbuild/protobuf/wkt";
import type { ServiceImpl } from "@connectrpc/connect";
import { afterEach, describe, expect, test } from "vitest";

import {
  CONNECT_STREAM_JSON,
  ParsedDescriptor,
  SchemaGenerator,
  Server,
  httpHandler,
  runCli,
} from "../src/index.js";
import { grpcServiceDefinitionForService } from "../src/grpc.js";
import { mcpDispatch } from "../src/mcp.js";
import { codeFromGrpcStatus, codeFromHttpStatus, grpcStatusFor, InvariantError } from "../src/errors.js";
import {
  GreetService,
  type GreetGroupRequest,
  type GreetRequest,
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
    const schema = new SchemaGenerator(ParsedDescriptor.fromFile(descriptorPath)).messageToSchema("greet.v1.GreetRequest");
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
      [400, 401, 403, 404, 429, 502, 503, 504, 418, 409, 499, 500, 501].map((status) =>
        codeFromHttpStatus(status),
      ),
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
    if (!request?.field[0]) {
      throw new Error("missing greet request descriptor");
    }
    request.field[0].name = "drifted_name";
    const registry = createFileRegistry(fds);
    const driftedService = [...registry].find(
      (desc) => desc.kind === "service" && desc.typeName === GreetService.typeName,
    );
    if (!driftedService || driftedService.kind !== "service") {
      throw new Error("missing drifted service");
    }

    const server = Server.fromDescriptor(descriptorPath);
    expect(() => server.register(driftedService, {})).toThrow(/Generated message .* does not match descriptor\.binpb/);
  });

  test("freezes registration, interceptors, filters, and limits on first execution", async () => {
    const server = registeredServer();
    await server.invoke("GreetService.Greet", { name: "Ada" });

    expect(() => server.register(GreetService, new GreetServicer())).toThrow(/cannot be changed after execution begins/);
    expect(() => server.use((next) => next)).toThrow(/cannot be changed after execution begins/);
    expect(() => server.include("*")).toThrow(/cannot be changed after execution begins/);
    expect(() => server.setMaxUnaryRequestBytes(1024)).toThrow(/cannot be changed after execution begins/);
    expect(() => server.configureMethod("/greet.v1.GreetService/Greet", {})).toThrow(
      /cannot be changed after execution begins/,
    );

    const grpcBuilt = registeredServer();
    const grpcServer = grpcBuilt.grpcServer();
    expect(() => grpcBuilt.exclude("*")).toThrow(/cannot be changed after execution begins/);
    expect(() => grpcBuilt.grpcServer()).toThrow(/already been created/);
    grpcBuilt.forceStop();

    const httpBuilt = registeredServer();
    httpHandler(httpBuilt);
    expect(() => httpBuilt.use((next) => next)).toThrow(
      /cannot be changed after execution begins/,
    );
  });

  test("requires projection filters before service registration", () => {
    const server = registeredServer();
    expect(() => server.include("*")).toThrow(/before service registration/);
    expect(() => server.exclude("*")).toThrow(/before service registration/);
  });

  test("rejects invalid global and per-method byte limits", () => {
    const server = registeredServer();
    expect(() => server.setMaxUnaryRequestBytes(0)).toThrow(/positive integer/);
    expect(() => server.setMaxUnaryResponseBytes(-1)).toThrow(/positive integer/);
    expect(() => server.setMaxStreamRequestBytes(Number.NaN)).toThrow(/positive integer/);
    expect(() => server.setMaxStreamResponseBytes(1.5)).toThrow(/positive integer/);
    expect(() =>
      server.configureMethod("/greet.v1.GreetService/Greet", { maxUnaryResponseBytes: 0 }),
    ).toThrow(/positive integer/);
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

    const mcp = await mcpDispatch(server, {
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "GreetService.Greet", arguments: { name: "Ada" } },
    });
    expect((mcp?.result as any).content[0].text).toContain("Hi Ada");
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

  test("enforces Connect streaming request and response limits per message", async () => {
    const requestLimited = registeredServer();
    requestLimited.setMaxStreamRequestBytes(32);
    const requestBase = await startHttp(requestLimited, servers);
    const oversizedRequest = connectEnvelope(
      0,
      Buffer.from(JSON.stringify({ name: "x".repeat(64), count: 1 })),
    );
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
    responseLimited.setMaxStreamResponseBytes(32);
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
    expect(JSON.parse(Buffer.from(responseFrames[0]!.payload).toString("utf8"))).toMatchObject({
      error: { code: "resource_exhausted" },
    });
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

    const protoPath = resolve(here, "../../node_modules/@grpc/reflection/build/proto/grpc/reflection/v1/reflection.proto");
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
    client.close();
  });

  test("proxies HTTP calls with google.api.http annotations, auth, and observer", async () => {
    const observed: any[] = [];
    const remote = createServer((req, res) => {
      const url = new URL(req.url ?? "/", "http://remote");
      if (req.method === "GET" && url.pathname === "/v1/greet/Ada") {
        expect(req.headers["x-test-auth"]).toBe("yes");
        res.setHeader("content-type", "application/json");
        res.end(JSON.stringify({ message: `REST ${url.pathname.split("/").at(-1)}`, mood: url.searchParams.get("mood") }));
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

async function startHttp(
  server: Server,
  servers: Array<ReturnType<typeof createServer>>,
): Promise<string> {
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
