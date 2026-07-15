import { createServer } from "node:http";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";
import { toBinary } from "@bufbuild/protobuf";
import { afterEach, describe, expect, test } from "vitest";

import {
  CONNECT_STREAM_JSON,
  ParsedDescriptor,
  SchemaGenerator,
  Server,
  buildGrpcServer,
  grpcClientForService,
  httpHandler,
  mcpDispatch,
  runCli,
} from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const descriptorPath = resolve(here, "../../python/tests/proto/descriptor.binpb");

class GreetServicer {
  async Greet(request: any) {
    return {
      message: `Hi ${request.name}`,
      mood: request.mood ?? 0,
      tags: request.tags ?? {},
    };
  }

  async GreetGroup(request: any) {
    return {
      messages: (request.people ?? []).map((person: any) => `Hi ${person.name}`),
      count: request.people?.length ?? 0,
    };
  }

  async *StreamGreet(request: any) {
    for (let i = 0; i < (request.count || 1); i += 1) {
      yield {
        message: `Hi ${request.name} #${i + 1}`,
        mood: 0,
        tags: {},
      };
    }
  }
}

function registeredServer(): Server {
  const server = Server.fromDescriptor(descriptorPath);
  server.register(new GreetServicer());
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
  test("registers unary and server-streaming handlers", async () => {
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
    server.use(async (request, context, info, next) => {
      calls.push(`before:${info.fullMethod}`);
      const response = await next(request, context);
      calls.push("after");
      return response;
    });

    await server.invoke("GreetService.Greet", { name: "Ada" });
    expect(calls).toEqual(["before:/greet.v1.GreetService/Greet", "after"]);
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
    expect((await descriptor.arrayBuffer()).byteLength).toBeGreaterThan(0);
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

  test("serves dynamic gRPC unary and server-streaming methods", async () => {
    const server = registeredServer();
    const started = await startGrpc(server);
    grpcServers.push(started.server);
    const tools = [...server.tools.values()].filter((tool) => tool.serviceFullName === "greet.v1.GreetService");
    const client = grpcClientForService(started.address, "greet.v1.GreetService", tools);

    const unary = await unaryCall(client, "Greet", { name: "Ada", mood: "MOOD_HAPPY" });
    expect(server.toJson(server.tools.get("GreetService.Greet")!, unary)).toEqual({
      message: "Hi Ada",
      mood: "MOOD_HAPPY",
    });

    const chunks = await streamCall(client, "StreamGreet", { name: "Ada", count: 2 });
    expect(chunks.map((chunk: any) => chunk.message)).toEqual(["Hi Ada #1", "Hi Ada #2"]);
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
    proxy.connect(started.address);
    const response = await proxy.invoke("GreetService.Greet", { name: "Ada" });
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, response)).toEqual({ message: "Hi Ada" });
    await proxy.stop();
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
      observer: (response) => observed.push(response),
    });
    const response = await proxy.invoke("GreetService.Greet", { name: "Ada", mood: "MOOD_HAPPY" });
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, response)).toEqual({
      message: "REST Ada",
      mood: "MOOD_HAPPY",
    });
    expect(observed).toHaveLength(1);
    expect(observed[0].request.url).toContain("/v1/greet/Ada");
  });
});

async function startGrpc(server: Server): Promise<{ server: grpc.Server; address: string }> {
  const grpcServer = buildGrpcServer(server);
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

function connectEnvelope(flags: number, data: Uint8Array): Buffer {
  const out = Buffer.alloc(5 + data.length);
  out[0] = flags;
  out.writeUInt32BE(data.length, 1);
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
