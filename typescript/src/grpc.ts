import {
  create,
  type DescMessage,
  type DescMethod,
  type DescService,
  fromBinary,
  fromJson,
  type JsonValue,
  type MessageShape,
  toBinary,
} from "@bufbuild/protobuf";
import { anyPack, FileDescriptorProtoSchema } from "@bufbuild/protobuf/wkt";
import { Code as ConnectCode, ConnectError, type ServiceImplSpec } from "@connectrpc/connect";
import * as grpc from "@grpc/grpc-js";
import type { PackageDefinition } from "@grpc/proto-loader";
import { ReflectionService } from "@grpc/reflection";

import { InvariantError, normalizeHandlerError, toConnectError } from "./errors.js";
import { StatusSchema } from "./gen/google/rpc/status_pb.js";
import {
  type ManagedHandlerContext,
  type RemoteServiceSpec,
  type Server,
  serverInternal,
  type UnaryHandler,
} from "./server.js";

export function createGrpcServer(server: Server, options?: grpc.ServerOptions): grpc.Server {
  server[serverInternal].freeze();
  const grpcServer = new grpc.Server(options);
  const serviceSpecs = server[serverInternal].registeredServiceSpecs();
  for (const spec of serviceSpecs) {
    grpcServer.addService(grpcServiceDefinitionForService(spec.service), grpcImplementation(server, spec));
  }
  const remoteServiceSpecs = server[serverInternal].registeredRemoteServiceSpecs();
  for (const spec of remoteServiceSpecs) {
    grpcServer.addService(grpcServiceDefinitionForRemoteService(spec), grpcRemoteImplementation(server, spec));
  }

  const reflectedMethods = new Map<string, Set<string>>();
  for (const spec of serviceSpecs) {
    reflectedMethods.set(spec.service.typeName, new Set(spec.service.methods.map((method) => method.name)));
  }
  for (const spec of remoteServiceSpecs) {
    reflectedMethods.set(
      spec.service.typeName,
      new Set(
        spec.service.methods.filter((method) => spec.handlers.has(method.localName)).map((method) => method.name),
      ),
    );
  }
  const serviceNames = [...reflectedMethods.keys()].sort();
  new ReflectionService(reflectionPackageDefinition(server, reflectedMethods), { services: serviceNames }).addToServer(
    grpcServer,
  );

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
  if (
    typeof value === "object" &&
    value !== null &&
    "$typeName" in value &&
    (value as { $typeName?: string }).$typeName === desc.typeName
  ) {
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
  defaultCallOptions: grpc.CallOptions = {},
): UnaryHandler {
  return (request, context) =>
    new Promise((resolve, reject) => {
      type UnaryClientMethod = (
        request: MessageShape<DescMessage>,
        metadata: grpc.Metadata,
        options: grpc.CallOptions,
        callback: (error: grpc.ServiceError | null, response: MessageShape<DescMessage>) => void,
      ) => grpc.ClientUnaryCall;
      const fn = (client as unknown as Record<string, UnaryClientMethod | undefined>)[methodName];
      if (fn === undefined) {
        reject(new InvariantError("internal", `gRPC client method ${methodName} is unavailable`));
        return;
      }
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
        if (response === undefined) {
          reject(new InvariantError("internal", `gRPC client method ${methodName} returned no response`));
          return;
        }
        resolve(response);
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
      out[method.localName] = (
        call: grpc.ServerWritableStream<MessageShape<DescMessage>, MessageShape<DescMessage>>,
      ) => {
        void (async () => {
          const scope = grpcHandlerContext(server, method, call);
          let headerSent = false;
          const sendHeader = (invalidBinaryCode: ConnectCode | null = ConnectCode.Internal) => {
            if (!headerSent) {
              call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader, invalidBinaryCode));
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
              await writeGrpcResponse(call, response, scope.context.signal);
            }
            sendHeader();
            call.end(grpcMetadataFromHeaders(scope.context.responseTrailer, ConnectCode.Internal));
          } catch (e) {
            sendHeader(null);
            call.destroy(toGrpcError(e, scope.context.responseTrailer, methodPath(method)));
          } finally {
            scope.finish();
          }
        })();
      };
    } else if (method.methodKind === "unary") {
      out[method.localName] = grpcUnaryImplementation(server, method, methodSpec.impl as UnaryHandler);
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
            call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader, ConnectCode.Internal));
            callback(null, response, grpcMetadataFromHeaders(scope.context.responseTrailer, ConnectCode.Internal));
          } catch (e) {
            call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader, null));
            callback(toGrpcError(e, scope.context.responseTrailer, methodPath(method)));
          } finally {
            scope.finish();
          }
        })();
      };
    } else {
      out[method.localName] = (call: grpc.ServerDuplexStream<MessageShape<DescMessage>, MessageShape<DescMessage>>) => {
        void (async () => {
          const scope = grpcHandlerContext(server, method, call);
          let headerSent = false;
          const sendHeader = (invalidBinaryCode: ConnectCode | null = ConnectCode.Internal) => {
            if (!headerSent) {
              call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader, invalidBinaryCode));
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
              await writeGrpcResponse(call, response, scope.context.signal);
            }
            sendHeader();
            call.end(grpcMetadataFromHeaders(scope.context.responseTrailer, ConnectCode.Internal));
          } catch (e) {
            sendHeader(null);
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

function grpcServiceDefinitionForRemoteService(spec: RemoteServiceSpec): grpc.ServiceDefinition {
  const out: Record<string, grpc.MethodDefinition<MessageShape<DescMessage>, MessageShape<DescMessage>>> = {};
  for (const method of spec.service.methods) {
    if (spec.handlers.has(method.localName)) {
      out[method.localName] = grpcMethodDefinition(method);
    }
  }
  return out;
}

function grpcRemoteImplementation(server: Server, spec: RemoteServiceSpec): grpc.UntypedServiceImplementation {
  const out: grpc.UntypedServiceImplementation = {};
  for (const method of spec.service.methods) {
    const handler = spec.handlers.get(method.localName);
    if (handler) {
      out[method.localName] = grpcUnaryImplementation(server, method, handler);
    }
  }
  return out;
}

function grpcUnaryImplementation(
  server: Server,
  method: DescMethod,
  handler: UnaryHandler,
): (
  call: grpc.ServerUnaryCall<MessageShape<DescMessage>, MessageShape<DescMessage>>,
  callback: grpc.sendUnaryData<MessageShape<DescMessage>>,
) => void {
  return (call, callback) => {
    void (async () => {
      const scope = grpcHandlerContext(server, method, call);
      try {
        const response = await server[serverInternal].invokeUnaryMethod(method, handler, call.request, scope.context);
        call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader, ConnectCode.Internal));
        callback(null, response, grpcMetadataFromHeaders(scope.context.responseTrailer, ConnectCode.Internal));
      } catch (e) {
        call.sendMetadata(grpcMetadataFromHeaders(scope.context.responseHeader, null));
        callback(toGrpcError(e, scope.context.responseTrailer, methodPath(method)));
      } finally {
        scope.finish();
      }
    })();
  };
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
  const requestHeader = headersFromGrpcMetadata(call.metadata);
  const context = server[serverInternal].createContext(method, {
    protocolName: "grpc",
    requestMethod: "POST",
    url: `grpc://${call.getHost()}${call.getPath()}`,
    timeoutMs: grpcTimeoutMs(call.getDeadline()),
    requestSignal: cancellation.signal,
    requestHeader,
  });
  Object.assign(context, {
    requestHeader,
    responseHeader: new GrpcMetadataHeaders(context.responseHeader),
    responseTrailer: new GrpcMetadataHeaders(context.responseTrailer),
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

async function writeGrpcResponse(
  call:
    | grpc.ServerWritableStream<MessageShape<DescMessage>, MessageShape<DescMessage>>
    | grpc.ServerDuplexStream<MessageShape<DescMessage>, MessageShape<DescMessage>>,
  response: MessageShape<DescMessage>,
  signal: AbortSignal,
): Promise<void> {
  if (signal.aborted || call.cancelled) {
    throw grpcCancellationError(signal);
  }
  if (call.write(response)) {
    return;
  }
  await new Promise<void>((resolve, reject) => {
    const cleanup = () => {
      call.off("drain", onDrain);
      call.off("error", onError);
      signal.removeEventListener("abort", onAbort);
    };
    const onDrain = () => {
      cleanup();
      resolve();
    };
    const onError = (error: Error) => {
      cleanup();
      reject(error);
    };
    const onAbort = () => {
      cleanup();
      reject(grpcCancellationError(signal));
    };
    call.once("drain", onDrain);
    call.once("error", onError);
    signal.addEventListener("abort", onAbort, { once: true });
    if (signal.aborted || call.cancelled) {
      onAbort();
    }
  });
}

function grpcCancellationError(signal: AbortSignal): unknown {
  return signal.reason ?? new ConnectError("request canceled", ConnectCode.Canceled);
}

function toGrpcError(err: unknown, responseTrailer: Headers | undefined, fullMethod: string): grpc.ServiceError {
  let connect = toConnectError(normalizeHandlerError(err, fullMethod));
  const trailerHeaders =
    responseTrailer instanceof GrpcMetadataHeaders ? responseTrailer.clone() : new GrpcMetadataHeaders(responseTrailer);
  connect.metadata.forEach((value, key) => {
    if (!trailerHeaders.has(key)) {
      trailerHeaders.append(key, value);
    }
  });
  let metadata: grpc.Metadata;
  try {
    metadata = grpcMetadataFromHeaders(trailerHeaders, ConnectCode.Internal);
  } catch (metadataError) {
    connect = toConnectError(normalizeHandlerError(metadataError, fullMethod));
    metadata = new grpc.Metadata();
  }
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

class GrpcMetadataHeaders extends Headers {
  readonly #values = new Map<string, string[]>();

  constructor(init?: HeadersInit) {
    super();
    if (init !== undefined) {
      new Headers(init).forEach((value, key) => {
        this.append(key, value);
      });
    }
  }

  override append(name: string, value: string): void {
    super.append(name, value);
    const key = name.toLowerCase();
    const values = this.#values.get(key) ?? [];
    values.push(String(value));
    this.#values.set(key, values);
  }

  override set(name: string, value: string): void {
    super.set(name, value);
    this.#values.set(name.toLowerCase(), [String(value)]);
  }

  override delete(name: string): void {
    super.delete(name);
    this.#values.delete(name.toLowerCase());
  }

  metadataValues(): ReadonlyMap<string, readonly string[]> {
    return this.#values;
  }

  clone(): GrpcMetadataHeaders {
    const clone = new GrpcMetadataHeaders();
    for (const [key, values] of this.#values) {
      for (const value of values) {
        clone.append(key, value);
      }
    }
    return clone;
  }
}

function headersFromGrpcMetadata(metadata: grpc.Metadata): Headers {
  const headers = new GrpcMetadataHeaders();
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
  for (const [key, values] of Object.entries(metadata.toJSON())) {
    if (reservedGrpcHeader(key)) {
      continue;
    }
    for (const value of values) {
      target.append(key, Buffer.isBuffer(value) ? value.toString("base64") : value);
    }
  }
}

function grpcMetadataFromHeaders(
  headers: Headers,
  invalidBinaryCode: ConnectCode | null = ConnectCode.InvalidArgument,
): grpc.Metadata {
  const metadata = new grpc.Metadata();
  const entries =
    headers instanceof GrpcMetadataHeaders
      ? headers.metadataValues()
      : new Map([...headers.entries()].map(([key, value]) => [key, [value]] as const));
  for (const [rawKey, values] of entries) {
    const key = rawKey.toLowerCase();
    if (reservedGrpcHeader(key)) {
      continue;
    }
    for (const value of values) {
      if (key.endsWith("-bin")) {
        for (const item of commaSeparatedBinaryMetadataValues(value)) {
          const decoded = decodeGrpcBinaryMetadata(item);
          if (decoded === undefined) {
            if (invalidBinaryCode !== null) {
              throw new ConnectError(`binary gRPC metadata '${key}' is not valid base64`, invalidBinaryCode);
            }
            continue;
          }
          metadata.add(key, decoded);
        }
      } else {
        metadata.add(key, value);
      }
    }
  }
  return metadata;
}

function commaSeparatedBinaryMetadataValues(value: string): string[] {
  return value.split(",").map((item) => item.trim());
}

function decodeGrpcBinaryMetadata(value: string): Buffer | undefined {
  const encoded = value.trim();
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(encoded)) {
    return undefined;
  }
  const unpadded = encoded.replace(/=+$/, "");
  const paddingLength = encoded.length - unpadded.length;
  const remainder = unpadded.length % 4;
  if (remainder === 1) {
    return undefined;
  }
  const expectedPaddingLength = remainder === 0 ? 0 : 4 - remainder;
  if (paddingLength !== 0 && paddingLength !== expectedPaddingLength) {
    return undefined;
  }
  const decoded = Buffer.from(unpadded, "base64");
  if (decoded.toString("base64").replace(/=+$/, "") !== unpadded) {
    return undefined;
  }
  return decoded;
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

function reflectionPackageDefinition(
  server: Server,
  reflectedMethods: ReadonlyMap<string, ReadonlySet<string>>,
): PackageDefinition {
  const out: Record<string, unknown> = {};
  for (const file of server.parsed.fds.file) {
    const reflectedFile = fromBinary(FileDescriptorProtoSchema, toBinary(FileDescriptorProtoSchema, file));
    reflectedFile.service = reflectedFile.service.filter((service) => {
      const serviceName = file.package ? `${file.package}.${service.name}` : service.name;
      const methods = reflectedMethods.get(serviceName);
      if (!methods) {
        return false;
      }
      service.method = service.method.filter((method) => methods.has(method.name));
      return true;
    });
    reflectedFile.sourceCodeInfo = undefined;
    out[reflectedFile.name || reflectedFile.package || "descriptor"] = {
      format: "Protocol Buffer 3 DescriptorProto",
      type: {},
      fileDescriptorProtos: [Buffer.from(toBinary(FileDescriptorProtoSchema, reflectedFile))],
    };
  }
  return out as PackageDefinition;
}
