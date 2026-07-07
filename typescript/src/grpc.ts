import * as grpc from "@grpc/grpc-js";
import { ReflectionService } from "@grpc/reflection";
import { create, fromBinary, fromJson, toBinary, type DescMessage, type JsonValue, type MessageShape } from "@bufbuild/protobuf";
import { FileDescriptorProtoSchema } from "@bufbuild/protobuf/wkt";
import type { PackageDefinition } from "@grpc/proto-loader";

import { asInvariantError, codeFromGrpcStatus, grpcStatusFor, InvariantError } from "./errors.js";
import { type Server, type Tool, type UnaryHandler } from "./server.js";

export type GrpcConnectOptions = {
  serviceName?: string;
  credentials?: grpc.ChannelCredentials;
  channelOptions?: Partial<grpc.ChannelOptions>;
};

export function buildGrpcServer(server: Server, options?: grpc.ServerOptions): grpc.Server {
  const grpcServer = new grpc.Server(options);
  for (const [serviceName, tools] of toolsByService(server)) {
    grpcServer.addService(grpcServiceDefinition(tools), grpcImplementation(server, tools));
  }

  const serviceNames = [...new Set([...server.tools.values()].map((tool) => tool.serviceFullName))].sort();
  if (serviceNames.length > 0) {
    new ReflectionService(reflectionPackageDefinition(server), { services: serviceNames }).addToServer(grpcServer);
  }

  return grpcServer;
}

export function serveGrpc(
  server: Server,
  port: number,
  host = "0.0.0.0",
  options?: grpc.ServerOptions,
): Promise<grpc.Server> {
  const grpcServer = buildGrpcServer(server, options);
  return new Promise((resolve, reject) => {
    grpcServer.bindAsync(`${host}:${port}`, grpc.ServerCredentials.createInsecure(), (err) => {
      if (err) {
        reject(err);
        return;
      }
      resolve(grpcServer);
    });
  });
}

export function grpcServiceDefinition(tools: Iterable<Tool>): grpc.ServiceDefinition {
  const out: Record<string, grpc.MethodDefinition<MessageShape<DescMessage>, MessageShape<DescMessage>>> = {};
  for (const tool of tools) {
    out[tool.methodName] = {
      path: `/${tool.serviceFullName}/${tool.methodName}`,
      requestStream: false,
      responseStream: tool.serverStreaming,
      requestSerialize: (message) => Buffer.from(toBinary(tool.inputDesc, messageForBinary(tool.inputDesc, message))),
      requestDeserialize: (bytes) => fromBinary(tool.inputDesc, bytes),
      responseSerialize: (message) => Buffer.from(toBinary(tool.outputDesc, messageForBinary(tool.outputDesc, message))),
      responseDeserialize: (bytes) => fromBinary(tool.outputDesc, bytes),
      originalName: tool.methodName,
    };
  }
  return out;
}

function messageForBinary(desc: DescMessage, value: unknown): MessageShape<DescMessage> {
  if (typeof value === "object" && value !== null && "$typeName" in value && (value as { $typeName?: string }).$typeName === desc.typeName) {
    return value as MessageShape<DescMessage>;
  }
  try {
    return fromJson(desc, (value ?? {}) as JsonValue);
  } catch {
    return create(desc, (value ?? {}) as never);
  }
}

export function grpcProxyHandler(client: grpc.Client, methodName: string, tool: Tool): UnaryHandler {
  return (request) =>
    new Promise((resolve, reject) => {
      const fn = (client as unknown as Record<string, Function>)[methodName];
      fn.call(client, request, (err: grpc.ServiceError | null, response: MessageShape<DescMessage>) => {
        if (err) {
          reject(new InvariantError(codeFromGrpcStatus(err.code), err.details || err.message));
          return;
        }
        resolve(response);
      });
    });
}

export function grpcClientForService(
  address: string,
  serviceName: string,
  tools: Tool[],
  options: GrpcConnectOptions = {},
): grpc.Client {
  const ClientCtor = grpc.makeGenericClientConstructor(grpcServiceDefinition(tools), serviceName);
  return new ClientCtor(
    address,
    options.credentials ?? grpc.credentials.createInsecure(),
    options.channelOptions,
  ) as grpc.Client;
}

function grpcImplementation(server: Server, tools: Tool[]): grpc.UntypedServiceImplementation {
  const out: grpc.UntypedServiceImplementation = {};
  for (const tool of tools) {
    if (tool.serverStreaming) {
      out[tool.methodName] = (call: grpc.ServerWritableStream<MessageShape<DescMessage>, MessageShape<DescMessage>>) => {
        void (async () => {
          try {
            for await (const response of server.invokeStreamTool(tool, call.request, call)) {
              call.write(response);
            }
            call.end();
          } catch (e) {
            call.destroy(toGrpcError(e));
          }
        })();
      };
    } else {
      out[tool.methodName] = (
        call: grpc.ServerUnaryCall<MessageShape<DescMessage>, MessageShape<DescMessage>>,
        callback: grpc.sendUnaryData<MessageShape<DescMessage>>,
      ) => {
        void (async () => {
          try {
            callback(null, await server.invokeTool(tool, call.request, call));
          } catch (e) {
            callback(toGrpcError(e));
          }
        })();
      };
    }
  }
  return out;
}

function toGrpcError(err: unknown): grpc.ServiceError {
  const inv = asInvariantError(err);
  return Object.assign(new Error(inv.message), {
    code: grpcStatusFor(inv.code),
    details: inv.message,
    metadata: new grpc.Metadata(),
  });
}

function toolsByService(server: Server): Map<string, Tool[]> {
  const services = new Map<string, Tool[]>();
  for (const tool of server.tools.values()) {
    const tools = services.get(tool.serviceFullName) ?? [];
    tools.push(tool);
    services.set(tool.serviceFullName, tools);
  }
  return services;
}

function reflectionPackageDefinition(server: Server): PackageDefinition {
  const out: Record<string, unknown> = {};
  for (const file of server.parsed.fds.file) {
    out[file.name || file.package || "descriptor"] = {
      format: "Protocol Buffer 3 DescriptorProto",
      type: {},
      fileDescriptorProtos: [Buffer.from(toBinary(FileDescriptorProtoSchema, file))],
    };
  }
  return out as PackageDefinition;
}
