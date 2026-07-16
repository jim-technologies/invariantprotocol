import * as grpc from "@grpc/grpc-js";
import { ReflectionService } from "@grpc/reflection";
import {
  create,
  fromBinary,
  fromJson,
  toBinary,
  type DescMessage,
  type DescMethod,
  type DescService,
  type JsonValue,
  type MessageShape,
} from "@bufbuild/protobuf";
import { Code as ConnectCode, ConnectError, type ServiceImplSpec } from "@connectrpc/connect";
import { anyPack, FileDescriptorProtoSchema } from "@bufbuild/protobuf/wkt";
import type { PackageDefinition } from "@grpc/proto-loader";

import { normalizeHandlerError, toConnectError } from "./errors.js";
import { StatusSchema } from "./gen/google/rpc/status_pb.js";
import {
  serverInternal,
  type ManagedHandlerContext,
  type Server,
  type Tool,
  type UnaryHandler,
} from "./server.js";

export function createGrpcServer(server: Server, options?: grpc.ServerOptions): grpc.Server {
  server[serverInternal].freeze();
  const grpcServer = new grpc.Server(options);
  const serviceSpecs = server[serverInternal].registeredServiceSpecs();
  for (const spec of serviceSpecs) {
    grpcServer.addService(grpcServiceDefinitionForService(spec.service), grpcImplementation(server, spec));
  }

  const serviceNames = serviceSpecs.map((spec) => spec.service.typeName).sort();
  new ReflectionService(reflectionPackageDefinition(server), { services: serviceNames }).addToServer(grpcServer);

  return grpcServer;
}

export function grpcServiceDefinitionForService(service: DescService): grpc.ServiceDefinition {
  const out: Record<string, grpc.MethodDefinition<MessageShape<DescMessage>, MessageShape<DescMessage>>> = {};
  for (const method of service.methods) {
    out[method.localName] = grpcMethodDefinition(method);
  }
  return out;
}

function grpcMethodDefinition(
  method: DescMethod,
): grpc.MethodDefinition<MessageShape<DescMessage>, MessageShape<DescMessage>> {
  return {
    path: `/${method.parent.typeName}/${method.name}`,
    requestStream: method.methodKind === "client_streaming" || method.methodKind === "bidi_streaming",
    responseStream: method.methodKind === "server_streaming" || method.methodKind === "bidi_streaming",
    requestSerialize: (message) => Buffer.from(toBinary(method.input, messageForBinary(method.input, message))),
    requestDeserialize: (bytes) => fromBinary(method.input, bytes),
    responseSerialize: (message) => Buffer.from(toBinary(method.output, messageForBinary(method.output, message))),
    responseDeserialize: (bytes) => fromBinary(method.output, bytes),
    originalName: method.name,
  };
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

export function grpcProxyHandler(
  client: grpc.Client,
  methodName: string,
  tool: Tool,
  defaultCallOptions: grpc.CallOptions = {},
): UnaryHandler {
  return (request, context) =>
    new Promise((resolve, reject) => {
      const fn = (client as unknown as Record<string, Function>)[methodName];
      const metadata = grpcMetadataFromHeaders(context.requestHeader);
      const timeoutMs = context.timeoutMs();
      const callOptions: grpc.CallOptions = { ...defaultCallOptions };
      if (timeoutMs !== undefined) {
        const contextDeadline = Date.now() + Math.max(0, timeoutMs);
        const configuredDeadline = callOptions.deadline;
        const configuredTimestamp =
          configuredDeadline instanceof Date ? configuredDeadline.getTime() : configuredDeadline;
        callOptions.deadline = new Date(
          configuredTimestamp === undefined ? contextDeadline : Math.min(contextDeadline, configuredTimestamp),
        );
      }

      let callbackDone = false;
      let statusDone = false;
      let callbackError: grpc.ServiceError | null = null;
      let response: MessageShape<DescMessage> | undefined;
      let statusMetadata: grpc.Metadata | undefined;
      const settle = () => {
        if (!callbackDone || !statusDone) {
          return;
        }
        context.signal.removeEventListener("abort", cancel);
        if (callbackError) {
          if (statusMetadata) {
            callbackError.metadata = callbackError.metadata?.clone() ?? new grpc.Metadata();
            for (const [key, values] of Object.entries(statusMetadata.toJSON())) {
              if (callbackError.metadata.get(key).length === 0) {
                for (const value of values) {
                  callbackError.metadata.add(key, value);
                }
              }
            }
          }
          reject(connectErrorFromGrpc(callbackError));
          return;
        }
        resolve(response!);
      };

      const call = fn.call(
        client,
        request,
        metadata,
        callOptions,
        (err: grpc.ServiceError | null, value: MessageShape<DescMessage>) => {
          callbackError = err;
          response = value;
          callbackDone = true;
          settle();
        },
      ) as grpc.ClientUnaryCall;
      const cancel = () => call.cancel();
      call.on("metadata", (incoming: grpc.Metadata) => appendGrpcMetadata(context.responseHeader, incoming));
      call.on("status", (status) => {
        statusMetadata = status.metadata;
        appendGrpcMetadata(context.responseTrailer, status.metadata);
        statusDone = true;
        settle();
      });
      if (context.signal.aborted) {
        cancel();
      } else {
        context.signal.addEventListener("abort", cancel, { once: true });
      }
    });
}

function grpcImplementation(server: Server, spec: ServiceImplSpec): grpc.UntypedServiceImplementation {
  const out: grpc.UntypedServiceImplementation = {};
  for (const method of spec.service.methods) {
    const methodSpec = spec.methods[method.localName];
    if (method.methodKind === "server_streaming") {
      out[method.localName] = (call: grpc.ServerWritableStream<MessageShape<DescMessage>, MessageShape<DescMessage>>) => {
        void (async () => {
          const scope = grpcHandlerContext(server, method, call);
          let headerSent = false;
          const sendHeader = () => {
            if (!headerSent) {
              call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader));
              headerSent = true;
            }
          };
          try {
            for await (const response of server[serverInternal].invokeServerStreamMethod(
              method,
              methodSpec.impl as never,
              call.request,
              scope.context,
            )) {
              sendHeader();
              call.write(response);
            }
            sendHeader();
            call.end(grpcMetadataFromHeaders(scope.context.responseTrailer));
          } catch (e) {
            sendHeader();
            call.destroy(toGrpcError(e, scope.context.responseTrailer, methodPath(method)));
          } finally {
            scope.finish();
          }
        })();
      };
    } else if (method.methodKind === "unary") {
      out[method.localName] = (
        call: grpc.ServerUnaryCall<MessageShape<DescMessage>, MessageShape<DescMessage>>,
        callback: grpc.sendUnaryData<MessageShape<DescMessage>>,
      ) => {
        void (async () => {
          const scope = grpcHandlerContext(server, method, call);
          try {
            const response = await server[serverInternal].invokeUnaryMethod(
              method,
              methodSpec.impl as never,
              call.request,
              scope.context,
            );
            call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader));
            callback(null, response, grpcMetadataFromHeaders(scope.context.responseTrailer));
          } catch (e) {
            call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader));
            callback(toGrpcError(e, scope.context.responseTrailer, methodPath(method)));
          } finally {
            scope.finish();
          }
        })();
      };
    } else if (method.methodKind === "client_streaming") {
      out[method.localName] = (
        call: grpc.ServerReadableStream<MessageShape<DescMessage>, MessageShape<DescMessage>>,
        callback: grpc.sendUnaryData<MessageShape<DescMessage>>,
      ) => {
        void (async () => {
          const scope = grpcHandlerContext(server, method, call);
          try {
            const response = await server[serverInternal].invokeClientStreamMethod(
              method,
              methodSpec.impl as never,
              call,
              scope.context,
            );
            call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader));
            callback(
              null,
              response,
              grpcMetadataFromHeaders(scope.context.responseTrailer),
            );
          } catch (e) {
            call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader));
            callback(toGrpcError(e, scope.context.responseTrailer, methodPath(method)));
          } finally {
            scope.finish();
          }
        })();
      };
    } else {
      out[method.localName] = (
        call: grpc.ServerDuplexStream<MessageShape<DescMessage>, MessageShape<DescMessage>>,
      ) => {
        void (async () => {
          const scope = grpcHandlerContext(server, method, call);
          let headerSent = false;
          const sendHeader = () => {
            if (!headerSent) {
              call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader));
              headerSent = true;
            }
          };
          try {
            for await (const response of server[serverInternal].invokeBidiStreamMethod(
              method,
              methodSpec.impl as never,
              call,
              scope.context,
            )) {
              sendHeader();
              call.write(response);
            }
            sendHeader();
            call.end(grpcMetadataFromHeaders(scope.context.responseTrailer));
          } catch (e) {
            sendHeader();
            call.destroy(toGrpcError(e, scope.context.responseTrailer, methodPath(method)));
          } finally {
            scope.finish();
          }
        })();
      };
    }
  }
  return out;
}

function grpcHandlerContext(
  server: Server,
  method: DescMethod,
  call: {
    getDeadline(): grpc.Deadline;
    getHost(): string;
    getPath(): string;
    readonly metadata: grpc.Metadata;
    off(event: string, listener: () => void): unknown;
    once(event: string, listener: () => void): unknown;
  },
): { context: ManagedHandlerContext; finish: () => void } {
  const cancellation = new AbortController();
  const cancel = () => cancellation.abort(new ConnectError("request canceled", ConnectCode.Canceled));
  call.once("cancelled", cancel);
  const context = server[serverInternal].createContext(method, {
    protocolName: "grpc",
    requestMethod: "POST",
    url: `grpc://${call.getHost()}${call.getPath()}`,
    timeoutMs: grpcTimeoutMs(call.getDeadline()),
    requestSignal: cancellation.signal,
    requestHeader: headersFromGrpcMetadata(call.metadata),
  });
  return {
    context,
    finish: () => {
      call.off("cancelled", cancel);
      context.abort();
    },
  };
}

function grpcTimeoutMs(deadline: grpc.Deadline): number | undefined {
  const timestamp = deadline instanceof Date ? deadline.getTime() : deadline;
  return Number.isFinite(timestamp) ? Math.max(0, timestamp - Date.now()) : undefined;
}

function toGrpcError(err: unknown, responseTrailer: Headers | undefined, fullMethod: string): grpc.ServiceError {
  const connect = toConnectError(normalizeHandlerError(err, fullMethod));
  const trailerHeaders = new Headers(responseTrailer);
  connect.metadata.forEach((value, key) => trailerHeaders.append(key, value));
  const metadata = grpcMetadataFromHeaders(trailerHeaders);
  if (connect.details.length > 0) {
    const status = create(StatusSchema, {
      code: connect.code,
      message: connect.rawMessage,
      details: connect.details.map((detail) =>
        "desc" in detail
          ? anyPack(detail.desc, create(detail.desc, detail.value))
          : { typeUrl: `type.googleapis.com/${detail.type}`, value: detail.value },
      ),
    });
    metadata.set("grpc-status-details-bin", Buffer.from(toBinary(StatusSchema, status)));
  }
  return Object.assign(new Error(connect.rawMessage), {
    code: connect.code as unknown as grpc.status,
    details: connect.rawMessage,
    metadata,
  });
}

function methodPath(method: DescMethod): string {
  return `/${method.parent.typeName}/${method.name}`;
}

function connectErrorFromGrpc(error: grpc.ServiceError): ConnectError {
  const trailer = headersFromGrpcMetadata(error.metadata);
  const encodedStatus = error.metadata.get("grpc-status-details-bin").find(Buffer.isBuffer);
  if (encodedStatus && Buffer.isBuffer(encodedStatus)) {
    try {
      const status = fromBinary(StatusSchema, encodedStatus);
      if (status.code === 0) {
        return new ConnectError("grpc-status-details-bin contains OK for an error", ConnectCode.DataLoss, trailer);
      }
      const rich = new ConnectError(status.message, status.code as ConnectCode, trailer);
      rich.details = status.details.map((detail) => ({
        type: detail.typeUrl.slice(detail.typeUrl.lastIndexOf("/") + 1),
        value: detail.value,
      }));
      return rich;
    } catch (decodeError) {
      return ConnectError.from(decodeError, ConnectCode.DataLoss);
    }
  }
  return new ConnectError(error.details || error.message, error.code as unknown as ConnectCode, trailer);
}

function headersFromGrpcMetadata(metadata: grpc.Metadata): Headers {
  const headers = new Headers();
  for (const [key, values] of Object.entries(metadata.toJSON())) {
    for (const value of values) {
      const encoded = Buffer.isBuffer(value) ? value.toString("base64") : value;
      if (key === "grpc-status-details-bin") {
        headers.set(key, encoded);
      } else {
        headers.append(key, encoded);
      }
    }
  }
  return headers;
}

function appendGrpcMetadata(target: Headers, metadata: grpc.Metadata): void {
  headersFromGrpcMetadata(metadata).forEach((value, key) => {
    if (!reservedGrpcHeader(key)) {
      target.append(key, value);
    }
  });
}

function grpcMetadataFromHeaders(headers: Headers): grpc.Metadata {
  const metadata = new grpc.Metadata();
  headers.forEach((value, rawKey) => {
    const key = rawKey.toLowerCase();
    if (reservedGrpcHeader(key)) {
      return;
    }
    if (key.endsWith("-bin")) {
      try {
        metadata.add(key, Buffer.from(value, "base64"));
      } catch {
        // Invalid binary metadata cannot be represented by grpc-js.
      }
      return;
    }
    metadata.add(key, value);
  });
  return metadata;
}

function reservedGrpcHeader(key: string): boolean {
  return (
    key === "content-type" ||
    key === "te" ||
    key === "user-agent" ||
    key === "grpc-status" ||
    key === "grpc-message" ||
    key.startsWith("grpc-")
  );
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
