import {
  clone,
  create,
  type DescEnum,
  type DescFile,
  type DescMessage,
  type DescMethod,
  type DescService,
  equals,
  fromJson,
  type JsonValue,
  type MessageShape,
  toJson,
} from "@bufbuild/protobuf";
import {
  DescriptorProtoSchema,
  EnumDescriptorProtoSchema,
  FileDescriptorProtoSchema,
  ServiceDescriptorProtoSchema,
} from "@bufbuild/protobuf/wkt";
import {
  Code as ConnectCode,
  ConnectError,
  type HandlerContext as ConnectHandlerContext,
  createServiceImplSpec,
  type Interceptor,
  type ServiceImpl,
  type ServiceImplSpec,
  type StreamRequest,
  type StreamResponse,
  type UnaryRequest,
  type UnaryResponse,
} from "@connectrpc/connect";
import type * as grpc from "@grpc/grpc-js";

import { createDeadlineHandlerContext } from "./deadline.js";
import { ParsedDescriptor, type ServiceInfo } from "./descriptor.js";
import { failedPrecondition, InvariantError, normalizeHandlerError, notFound } from "./errors.js";
import { createGrpcServer, grpcProxyHandler } from "./grpc.js";
import { httpHandler as buildHttpHandler, serveHttp as serveHttpProjection } from "./http.js";
import {
  type ConnectHttpOptions,
  clientBindingForMethod,
  HTTPConnection,
  httpProxyHandler,
  httpRulesByMethodPath,
} from "./http_client.js";
import { type JsonSchema, SchemaGenerator } from "./schema.js";

export type HandlerContext = ConnectHandlerContext;
export type ManagedHandlerContext = HandlerContext & { abort(reason?: unknown): void };
export type ProjectionContextOptions = {
  protocolName: string;
  requestMethod?: string;
  url?: string;
  timeoutMs?: number;
  requestSignal?: AbortSignal;
  requestHeader?: HeadersInit;
};
export type HttpMetadataMapper = (requestHeaders: Readonly<Headers>) => HeadersInit;
export type UnaryHandler = (request: MessageShape<DescMessage>, context: HandlerContext) => Promise<unknown> | unknown;
export type StreamHandler = (request: MessageShape<DescMessage>, context: HandlerContext) => AsyncIterable<unknown>;
export type RemoteServiceSpec = {
  service: DescService;
  handlers: ReadonlyMap<string, UnaryHandler>;
};

/** Immutable discovery metadata for one registered projection method. */
export type Tool = Readonly<{
  name: string;
  description: string;
  inputSchema: Readonly<JsonSchema>;
  inputType: string;
  outputType: string;
  serviceFullName: string;
  methodName: string;
  serverStreaming: boolean;
}>;

/** @internal Executable registration retained only by Invariant's adapters. */
export type RegisteredTool = {
  readonly name: string;
  readonly description: string;
  readonly inputSchema: JsonSchema;
  readonly handler: UnaryHandler | StreamHandler;
  readonly inputType: string;
  readonly outputType: string;
  readonly serviceFullName: string;
  readonly methodName: string;
  readonly serverStreaming: boolean;
  readonly inputDesc: DescMessage;
  readonly outputDesc: DescMessage;
  readonly methodDesc: DescMethod;
};

type MutableRegisteredTool = {
  name: string;
  description: string;
  inputSchema: JsonSchema;
  handler: UnaryHandler | StreamHandler;
  inputType: string;
  outputType: string;
  serviceFullName: string;
  methodName: string;
  serverStreaming: boolean;
  inputDesc: DescMessage;
  outputDesc: DescMessage;
  methodDesc: DescMethod;
};

export type MethodConfig = {
  maxUnaryRequestBytes?: number;
  maxUnaryResponseBytes?: number;
  maxStreamRequestBytes?: number;
  maxStreamResponseBytes?: number;
};

export type ToolCatalogEntry = Readonly<{
  name: string;
  description: string;
  inputSchema: Readonly<JsonSchema>;
  _meta?: Readonly<Record<string, unknown>>;
}>;

export const SERVER_NAME = "invariant-protocol";
export const SERVER_VERSION = "0.16.2";
const DEFAULT_HTTP_MESSAGE_BYTES = 16 * 1024 * 1024;
const DEFAULT_HTTP_METADATA_KEYS = ["traceparent", "tracestate", "baggage", "x-request-id"] as const;

export function defaultHttpMetadataMapper(requestHeaders: Readonly<Headers>): Headers {
  const mapped = new Headers();
  for (const key of DEFAULT_HTTP_METADATA_KEYS) {
    const value = requestHeaders.get(key);
    if (value !== null) {
      mapped.set(key, value);
    }
  }
  return mapped;
}

export const serverInternal = Symbol("invariant.server.internal");

export class Server {
  readonly name = SERVER_NAME;
  readonly version = SERVER_VERSION;
  readonly #parsed: ParsedDescriptor;
  readonly #schemaGen: SchemaGenerator;
  readonly #toolStore = new Map<string, RegisteredTool>();

  private readonly interceptors: Interceptor[] = [];
  private readonly services = new Map<string, ServiceImplSpec>();
  private readonly remoteServices = new Map<string, RemoteServiceSpec>();
  private readonly methodConfigs = new Map<string, MethodConfig>();
  private includes: string[] = [];
  private excludes: string[] = [];
  private httpMaxUnaryRequest = DEFAULT_HTTP_MESSAGE_BYTES;
  private httpMaxUnaryResponse = DEFAULT_HTTP_MESSAGE_BYTES;
  private connectStreamMaxRequest = DEFAULT_HTTP_MESSAGE_BYTES;
  private connectStreamMaxResponse = DEFAULT_HTTP_MESSAGE_BYTES;
  private httpMetadataMapper: HttpMetadataMapper = defaultHttpMetadataMapper;
  private grpcServerInstance: grpc.Server | undefined;
  private frozen = false;
  readonly [serverInternal] = {
    freeze: () => this.freezeConfiguration(),
    parsed: () => this.#parsed,
    tool: (name: string): RegisteredTool | undefined => this.#toolStore.get(name),
    tools: (): readonly RegisteredTool[] => [...this.#toolStore.values()],
    registeredServiceSpecs: (): readonly ServiceImplSpec[] => [...this.services.values()],
    registeredRemoteServiceSpecs: (): readonly RemoteServiceSpec[] => [...this.remoteServices.values()],
    coerceMessage: (desc: DescMessage, value: unknown) => this.#coerceMessage(desc, value),
    invokeTool: (tool: RegisteredTool, request: MessageShape<DescMessage>, context?: HandlerContext) =>
      this.#invokeTool(tool, request, context),
    invokeStreamTool: (tool: RegisteredTool, request: MessageShape<DescMessage>, context?: HandlerContext) =>
      this.#invokeStreamTool(tool, request, context),
    invokeUnaryMethod: (
      method: DescMethod,
      handler: UnaryHandler,
      request: MessageShape<DescMessage>,
      context: HandlerContext,
    ) => this.#invokeUnaryMethod(method, handler, request, context),
    invokeServerStreamMethod: (
      method: DescMethod,
      handler: StreamHandler,
      request: MessageShape<DescMessage>,
      context: HandlerContext,
    ) => this.#invokeServerStreamMethod(method, handler, request, context),
    invokeClientStreamMethod: (
      method: DescMethod,
      handler: (requests: AsyncIterable<MessageShape<DescMessage>>, context: HandlerContext) => Promise<unknown>,
      requests: AsyncIterable<MessageShape<DescMessage>>,
      context: HandlerContext,
    ) => this.#invokeClientStreamMethod(method, handler, requests, context),
    invokeBidiStreamMethod: (
      method: DescMethod,
      handler: (requests: AsyncIterable<MessageShape<DescMessage>>, context: HandlerContext) => AsyncIterable<unknown>,
      requests: AsyncIterable<MessageShape<DescMessage>>,
      context: HandlerContext,
    ) => this.#invokeBidiStreamMethod(method, handler, requests, context),
    createContext: (method: DescMethod, options: ProjectionContextOptions) => this.createContext(method, options),
    mapHTTPContext: (context: HandlerContext) => this.mapHTTPContext(context),
  };

  private constructor(parsed: ParsedDescriptor) {
    this.#parsed = parsed;
    this.#schemaGen = new SchemaGenerator(parsed);
  }

  static fromDescriptor(path: string): Server {
    return new Server(ParsedDescriptor.fromFile(path));
  }

  static fromBytes(bytes: Uint8Array): Server {
    return new Server(ParsedDescriptor.fromBytes(bytes));
  }

  /** Return a detached descriptor view that cannot mutate this server's registry. */
  get parsed(): ParsedDescriptor {
    return ParsedDescriptor.fromBytes(this.#parsed.bytes);
  }

  /** Return a schema generator backed by a detached descriptor view. */
  get schemaGen(): SchemaGenerator {
    return new SchemaGenerator(this.parsed);
  }

  /** Return a read-only metadata snapshot without executable handlers. */
  get tools(): ReadonlyMap<string, Tool> {
    return readOnlyMap(new Map([...this.#toolStore].map(([name, tool]) => [name, publicTool(tool)])));
  }

  include(...patterns: string[]): void {
    this.assertProjectionFiltersMutable();
    this.includes.push(...patterns);
  }

  exclude(...patterns: string[]): void {
    this.assertProjectionFiltersMutable();
    this.excludes.push(...patterns);
  }

  use(interceptor: Interceptor): void {
    this.assertMutable("shared interceptors");
    this.interceptors.push(interceptor);
  }

  setMaxUnaryRequestBytes(n: number): void {
    this.assertMutable("HTTP unary request limit");
    this.httpMaxUnaryRequest = n === 0 ? DEFAULT_HTTP_MESSAGE_BYTES : positiveByteLimit(n, "HTTP unary request limit");
  }

  setMaxUnaryResponseBytes(n: number): void {
    this.assertMutable("HTTP unary response limit");
    this.httpMaxUnaryResponse =
      n === 0 ? DEFAULT_HTTP_MESSAGE_BYTES : positiveByteLimit(n, "HTTP unary response limit");
  }

  setMaxStreamRequestBytes(n: number): void {
    this.assertMutable("HTTP stream request limit");
    this.connectStreamMaxRequest =
      n === 0 ? DEFAULT_HTTP_MESSAGE_BYTES : positiveByteLimit(n, "HTTP stream request limit");
  }

  setMaxStreamResponseBytes(n: number): void {
    this.assertMutable("HTTP stream response limit");
    this.connectStreamMaxResponse =
      n === 0 ? DEFAULT_HTTP_MESSAGE_BYTES : positiveByteLimit(n, "HTTP stream response limit");
  }

  configureMethod(fullMethod: string, config: MethodConfig): void {
    this.assertMutable("method configuration");
    for (const [name, value] of Object.entries(config)) {
      if (value !== undefined && value !== 0) {
        positiveByteLimit(value, `${fullMethod} ${name}`);
      }
    }
    this.methodConfigs.set(fullMethod, { ...config });
  }

  useHttpMetadataMapper(mapper: HttpMetadataMapper = defaultHttpMetadataMapper): void {
    this.assertMutable("HTTP metadata mapper");
    this.httpMetadataMapper = mapper;
  }

  maxUnaryRequestBytes(tool?: Tool): number {
    return this.methodLimit(tool, "maxUnaryRequestBytes", this.httpMaxUnaryRequest);
  }

  maxUnaryResponseBytes(tool?: Tool): number {
    return this.methodLimit(tool, "maxUnaryResponseBytes", this.httpMaxUnaryResponse);
  }

  maxStreamRequestBytes(tool?: Tool): number {
    return this.methodLimit(tool, "maxStreamRequestBytes", this.connectStreamMaxRequest);
  }

  maxStreamResponseBytes(tool?: Tool): number {
    return this.methodLimit(tool, "maxStreamResponseBytes", this.connectStreamMaxResponse);
  }

  register<T extends DescService>(service: T, implementation: Partial<ServiceImpl<T>>): void {
    this.assertMutable("service registration");
    const descriptorService = this.validateGeneratedService(service);
    if (this.services.has(service.typeName) || this.remoteServices.has(service.typeName)) {
      throw new Error(`Service ${service.typeName} is already registered.`);
    }

    const spec = createServiceImplSpec(service, implementation);
    const stagedTools: MutableRegisteredTool[] = [];

    for (const method of service.methods) {
      const methodInfo = descriptorService.methods.get(method.name);
      if (!methodInfo) {
        throw new InvariantError("internal", `missing method descriptor for ${service.typeName}.${method.name}`);
      }
      if (methodInfo.clientStreaming || !this.shouldInclude(service.typeName, method.name)) {
        continue;
      }

      const methodSpec = spec.methods[method.localName];
      stagedTools.push({
        name: `${service.typeName}.${method.name}`,
        description: methodInfo.comment || `${service.typeName}.${method.name}`,
        inputSchema: this.#schemaGen.messageToSchema(method.input.typeName),
        handler: methodSpec.impl as UnaryHandler | StreamHandler,
        inputType: method.input.typeName,
        outputType: method.output.typeName,
        serviceFullName: service.typeName,
        methodName: method.name,
        serverStreaming: methodInfo.serverStreaming,
        inputDesc: method.input,
        outputDesc: method.output,
        methodDesc: method,
      });
    }
    this.assertToolsAvailable(stagedTools);
    this.services.set(service.typeName, spec);
    for (const tool of stagedTools) {
      this.addTool(tool);
    }
  }

  connectGrpc<T extends DescService>(service: T, client: grpc.Client, defaultCallOptions: grpc.CallOptions = {}): void {
    this.assertMutable("gRPC proxy registration");
    const descriptorService = this.validateGeneratedService(service);
    if (this.services.has(service.typeName) || this.remoteServices.has(service.typeName)) {
      throw new Error(`Service ${service.typeName} is already registered.`);
    }
    const proxyTools: MutableRegisteredTool[] = [];
    const handlers = new Map<string, UnaryHandler>();
    for (const method of service.methods) {
      const methodInfo = descriptorService.methods.get(method.name);
      if (!methodInfo) {
        throw new InvariantError("internal", `missing method descriptor for ${service.typeName}.${method.name}`);
      }
      if (methodInfo.clientStreaming || methodInfo.serverStreaming) {
        continue;
      }
      if (typeof (client as unknown as Record<string, unknown>)[method.localName] !== "function") {
        throw new Error(`gRPC client does not implement ${service.typeName}.${method.localName}.`);
      }
      const tool: MutableRegisteredTool = {
        name: `${service.typeName}.${method.name}`,
        description: methodInfo.comment || `${service.typeName}.${method.name}`,
        inputSchema: this.#schemaGen.messageToSchema(method.input.typeName),
        handler: async () => {
          throw new InvariantError("internal", "gRPC proxy handler not initialized");
        },
        inputType: method.input.typeName,
        outputType: method.output.typeName,
        serviceFullName: service.typeName,
        methodName: method.name,
        serverStreaming: false,
        inputDesc: method.input,
        outputDesc: method.output,
        methodDesc: method,
      };
      tool.handler = grpcProxyHandler(client, method.localName, defaultCallOptions);
      handlers.set(method.localName, tool.handler as UnaryHandler);
      if (this.shouldInclude(service.typeName, method.name)) {
        proxyTools.push(tool);
      }
    }
    this.assertToolsAvailable(proxyTools);
    if (handlers.size > 0) {
      this.remoteServices.set(service.typeName, { service, handlers });
    }
    for (const tool of proxyTools) {
      this.addTool(tool);
    }
  }

  connectHttp(baseUrl: string, options: ConnectHttpOptions = {}): void {
    this.assertMutable("HTTP proxy registration");
    const services = options.serviceName ? this.serviceByName(options.serviceName) : this.#parsed.services;
    const rules = httpRulesByMethodPath(this);
    const connection = new HTTPConnection(baseUrl, options);
    const stagedTools: MutableRegisteredTool[] = [];
    const stagedServices: RemoteServiceSpec[] = [];

    for (const [svcFullName, svc] of services) {
      const handlers = new Map<string, UnaryHandler>();
      for (const [methodName, method] of svc.methods) {
        if (method.clientStreaming || method.serverStreaming) {
          continue;
        }
        const inputDesc = this.#parsed.getMessage(method.inputType);
        const outputDesc = this.#parsed.getMessage(method.outputType);
        if (!inputDesc || !outputDesc) {
          throw new InvariantError("internal", `missing message descriptor for ${svc.name}.${methodName}`);
        }

        const methodPath = `/${svcFullName}/${methodName}`;
        const binding = clientBindingForMethod(rules.get(methodPath), svcFullName, methodName);
        const tool: MutableRegisteredTool = {
          name: `${svcFullName}.${methodName}`,
          description: method.comment || `${svcFullName}.${methodName}`,
          inputSchema: this.#schemaGen.messageToSchema(method.inputType),
          handler: async () => {
            throw new InvariantError("internal", "HTTP proxy handler not initialized");
          },
          inputType: method.inputType,
          outputType: method.outputType,
          serviceFullName: svcFullName,
          methodName,
          serverStreaming: false,
          inputDesc,
          outputDesc,
          methodDesc: method.desc,
        };
        tool.handler = httpProxyHandler(this, connection, binding, tool, methodPath);
        handlers.set(method.desc.localName, tool.handler as UnaryHandler);
        if (this.shouldInclude(svcFullName, methodName)) {
          stagedTools.push(tool);
        }
      }
      if (handlers.size > 0) {
        stagedServices.push({ service: svc.desc, handlers });
      }
    }
    for (const spec of stagedServices) {
      if (this.services.has(spec.service.typeName) || this.remoteServices.has(spec.service.typeName)) {
        throw new Error(`Service ${spec.service.typeName} is already registered.`);
      }
    }
    this.assertToolsAvailable(stagedTools);
    for (const spec of stagedServices) {
      this.remoteServices.set(spec.service.typeName, spec);
    }
    for (const tool of stagedTools) {
      this.addTool(tool);
    }
  }

  async invoke(toolName: string, request: unknown, context?: HandlerContext): Promise<MessageShape<DescMessage>> {
    this.freezeConfiguration();
    const tool = this.#toolStore.get(toolName);
    if (!tool) {
      throw notFound(`Unknown tool '${toolName}'. Available: ${JSON.stringify([...this.#toolStore.keys()].sort())}`);
    }
    if (tool.serverStreaming) {
      throw failedPrecondition(`Tool '${toolName}' is server-streaming; use invokeStream`);
    }
    return this.#invokeTool(tool, this.#coerceMessage(tool.inputDesc, request), context);
  }

  async *invokeStream(
    toolName: string,
    request: unknown,
    context?: HandlerContext,
  ): AsyncIterable<MessageShape<DescMessage>> {
    this.freezeConfiguration();
    const tool = this.#toolStore.get(toolName);
    if (!tool) {
      throw notFound(`Unknown tool '${toolName}'. Available: ${JSON.stringify([...this.#toolStore.keys()].sort())}`);
    }
    if (!tool.serverStreaming) {
      throw failedPrecondition(`Tool '${toolName}' is unary; use invoke`);
    }
    yield* this.#invokeStreamTool(tool, this.#coerceMessage(tool.inputDesc, request), context);
  }

  toolCatalog(): readonly ToolCatalogEntry[] {
    return Object.freeze(
      [...this.#toolStore.values()]
        .sort((a, b) => a.name.localeCompare(b.name))
        .map(
          (tool) =>
            deepFreeze({
              name: tool.name,
              description: tool.description,
              inputSchema: structuredClone(tool.inputSchema),
              ...(tool.serverStreaming ? { _meta: { streaming: true } } : {}),
            }) as ToolCatalogEntry,
        ),
    );
  }

  httpHandler(): ReturnType<typeof buildHttpHandler> {
    this.freezeConfiguration();
    return buildHttpHandler(this);
  }

  serveHttp(port: number, host?: string) {
    this.freezeConfiguration();
    return serveHttpProjection(this, port, host);
  }

  grpcServer(options?: grpc.ServerOptions): grpc.Server {
    if (this.grpcServerInstance) {
      throw new Error("Invariant's native gRPC server has already been created.");
    }
    this.freezeConfiguration();
    const server = createGrpcServer(this, options);
    this.grpcServerInstance = server;
    return server;
  }

  stop(): Promise<void> {
    const server = this.grpcServerInstance;
    if (!server) {
      return Promise.resolve();
    }
    return new Promise((resolve, reject) => {
      server.tryShutdown((error) => {
        if (error) {
          reject(error);
        } else {
          resolve();
        }
      });
    });
  }

  forceStop(): void {
    this.grpcServerInstance?.forceShutdown();
  }

  toJson(toolName: string, message: MessageShape<DescMessage>): JsonValue {
    const tool = this.#toolStore.get(toolName);
    if (!tool) {
      throw notFound(`Unknown tool '${toolName}'.`);
    }
    return toJson(tool.outputDesc, message, { registry: this.#parsed.registry });
  }

  #coerceMessage(desc: DescMessage, value: unknown): MessageShape<DescMessage> {
    if (isMessageFor(desc, value)) {
      return value as MessageShape<DescMessage>;
    }
    try {
      return fromJson(desc, (value ?? {}) as JsonValue, { registry: this.#parsed.registry });
    } catch {
      return create(desc, (value ?? {}) as never);
    }
  }

  async #invokeTool(
    tool: RegisteredTool,
    request: MessageShape<DescMessage>,
    context?: HandlerContext,
  ): Promise<MessageShape<DescMessage>> {
    return this.#invokeUnaryMethod(tool.methodDesc, tool.handler as UnaryHandler, request, context);
  }

  async *#invokeStreamTool(
    tool: RegisteredTool,
    request: MessageShape<DescMessage>,
    context?: HandlerContext,
  ): AsyncIterable<MessageShape<DescMessage>> {
    yield* this.#invokeServerStreamMethod(tool.methodDesc, tool.handler as StreamHandler, request, context);
  }

  async #invokeUnaryMethod(
    method: DescMethod,
    handler: UnaryHandler,
    request: MessageShape<DescMessage>,
    context?: HandlerContext,
  ): Promise<MessageShape<DescMessage>> {
    this.freezeConfiguration();
    const fullMethod = `/${method.parent.typeName}/${method.name}`;
    const ownedContext = context === undefined ? this.createContext(method, { protocolName: "in-process" }) : undefined;
    const callContext = ownedContext ?? context;
    if (callContext === undefined) {
      throw new InvariantError("internal", `could not create a handler context for ${fullMethod}`);
    }
    try {
      const response = await runWithContext(callContext, () =>
        this.#interceptUnary(method, request, callContext, handler),
      );
      return this.#coerceMessage(method.output, response.message);
    } catch (error) {
      throw normalizeHandlerError(error, fullMethod);
    } finally {
      ownedContext?.abort();
    }
  }

  async *#invokeServerStreamMethod(
    method: DescMethod,
    handler: StreamHandler,
    request: MessageShape<DescMessage>,
    context?: HandlerContext,
  ): AsyncIterable<MessageShape<DescMessage>> {
    const streamingHandler = (
      requests: AsyncIterable<MessageShape<DescMessage>>,
      callContext: HandlerContext,
    ): AsyncIterable<unknown> =>
      (async function* () {
        const input = await exactlyOneMessage(requests, "server-streaming call");
        yield* handler(input, callContext);
      })();
    yield* this.#invokeStreamingMethod(method, streamingHandler, oneMessage(request), context);
  }

  async #invokeClientStreamMethod(
    method: DescMethod,
    handler: (requests: AsyncIterable<MessageShape<DescMessage>>, context: HandlerContext) => Promise<unknown>,
    requests: AsyncIterable<MessageShape<DescMessage>>,
    context?: HandlerContext,
  ): Promise<MessageShape<DescMessage>> {
    const streamingHandler = (
      input: AsyncIterable<MessageShape<DescMessage>>,
      callContext: HandlerContext,
    ): AsyncIterable<unknown> =>
      (async function* () {
        yield await handler(input, callContext);
      })();
    return exactlyOneMessage(
      this.#invokeStreamingMethod(method, streamingHandler, requests, context),
      "client-streaming call",
    );
  }

  async *#invokeBidiStreamMethod(
    method: DescMethod,
    handler: (requests: AsyncIterable<MessageShape<DescMessage>>, context: HandlerContext) => AsyncIterable<unknown>,
    requests: AsyncIterable<MessageShape<DescMessage>>,
    context?: HandlerContext,
  ): AsyncIterable<MessageShape<DescMessage>> {
    yield* this.#invokeStreamingMethod(method, handler, requests, context);
  }

  async *#invokeStreamingMethod(
    method: DescMethod,
    handler: (requests: AsyncIterable<MessageShape<DescMessage>>, context: HandlerContext) => AsyncIterable<unknown>,
    requests: AsyncIterable<MessageShape<DescMessage>>,
    context?: HandlerContext,
  ): AsyncIterable<MessageShape<DescMessage>> {
    this.freezeConfiguration();
    const fullMethod = `/${method.parent.typeName}/${method.name}`;
    const ownedContext = context === undefined ? this.createContext(method, { protocolName: "in-process" }) : undefined;
    const callContext = ownedContext ?? context;
    if (callContext === undefined) {
      throw new InvariantError("internal", `could not create a handler context for ${fullMethod}`);
    }
    try {
      const response = await runWithContext(callContext, () =>
        this.#interceptStream(method, requests, callContext, handler),
      );
      copyHeaders(response.header, callContext.responseHeader);
      const iterator = response.message[Symbol.asyncIterator]();
      let complete = false;
      try {
        for (;;) {
          const item = await runWithContext(callContext, () => iterator.next());
          if (item.done) {
            complete = true;
            break;
          }
          yield this.#coerceMessage(method.output, item.value);
        }
      } finally {
        if (!complete) {
          const closing = iterator.return?.();
          if (closing !== undefined) {
            const remaining = callContext.timeoutMs();
            if (callContext.signal.aborted || (remaining !== undefined && remaining <= 0)) {
              // JavaScript cannot forcibly stop a handler that ignores its
              // signal. Do request generator cleanup, but do not let that
              // uncooperative handler delay cancellation indefinitely.
              void closing.catch(() => undefined);
            } else {
              await closing;
            }
          }
        }
      }
      copyHeaders(response.trailer, callContext.responseTrailer);
    } catch (error) {
      throw normalizeHandlerError(error, fullMethod);
    } finally {
      ownedContext?.abort();
    }
  }

  private addTool(tool: MutableRegisteredTool): void {
    const existing = this.#toolStore.get(tool.name);
    if (existing) {
      throw new Error(
        `Tool ${tool.name} is already registered by ${existing.serviceFullName}; cannot register ${tool.serviceFullName}.`,
      );
    }
    this.#toolStore.set(tool.name, Object.freeze(tool));
  }

  private assertToolsAvailable(tools: readonly Pick<RegisteredTool, "name" | "serviceFullName">[]): void {
    const staged = new Map<string, string>();
    for (const tool of tools) {
      const owner = this.#toolStore.get(tool.name)?.serviceFullName ?? staged.get(tool.name);
      if (owner !== undefined) {
        throw new Error(
          `Tool ${tool.name} is already registered by ${owner}; cannot register ${tool.serviceFullName}.`,
        );
      }
      staged.set(tool.name, tool.serviceFullName);
    }
  }

  private assertMutable(subject: string): void {
    if (this.frozen) {
      throw new Error(`Invariant server ${subject} cannot be changed after execution begins.`);
    }
  }

  private assertProjectionFiltersMutable(): void {
    this.assertMutable("projection filters");
    if (this.services.size > 0 || this.remoteServices.size > 0 || this.#toolStore.size > 0) {
      throw new Error("Invariant projection filters must be configured before service registration.");
    }
  }

  private freezeConfiguration(): void {
    this.frozen = true;
  }

  private methodLimit(tool: Tool | undefined, key: keyof MethodConfig, fallback: number): number {
    if (!tool) {
      return fallback;
    }
    const config = this.methodConfigs.get(`/${tool.serviceFullName}/${tool.methodName}`);
    const value = config?.[key];
    return value === undefined || value === 0 ? fallback : value;
  }

  private createContext(method: DescMethod, options: ProjectionContextOptions): ManagedHandlerContext {
    return createDeadlineHandlerContext({
      service: method.parent,
      method,
      protocolName: options.protocolName,
      requestMethod: options.requestMethod ?? "POST",
      url: options.url ?? `${options.protocolName}:///${method.parent.typeName}/${method.name}`,
      timeoutMs: options.timeoutMs,
      requestSignal: options.requestSignal,
      requestHeader: options.requestHeader,
    });
  }

  private mapHTTPContext(context: HandlerContext): HandlerContext {
    const mapped = new Headers(this.httpMetadataMapper(new Headers(context.requestHeader)));
    const safe = new Headers();
    mapped.forEach((value, rawKey) => {
      const key = rawKey.trim().toLowerCase();
      if (validMetadataKey(key) && !reservedInboundMetadata(key) && validMetadataValue(value)) {
        safe.append(key, value);
      }
    });
    // The Connect context is deliberately retained so its cancellation,
    // response headers/trailers, timeout, and context values keep their normal
    // behavior. Only the untrusted incoming header view is replaced.
    Object.assign(context, { requestHeader: safe });
    return context;
  }

  async #interceptUnary(
    method: DescMethod,
    message: MessageShape<DescMessage>,
    context: HandlerContext,
    handler: UnaryHandler,
  ): Promise<UnaryResponse> {
    let next = async (request: UnaryRequest): Promise<UnaryResponse> => ({
      stream: false,
      service: method.parent,
      method: method as UnaryRequest["method"],
      header: context.responseHeader,
      trailer: context.responseTrailer,
      message: this.#coerceMessage(
        method.output,
        await handler(request.message, handlerContextForRequest(context, request)),
      ),
    });
    for (const interceptor of [...this.interceptors].reverse()) {
      next = interceptor(next as unknown as Parameters<Interceptor>[0]) as typeof next;
    }
    const response = await next({
      stream: false,
      service: method.parent,
      method: method as UnaryRequest["method"],
      requestMethod: context.requestMethod,
      url: context.url,
      signal: context.signal,
      header: context.requestHeader,
      contextValues: context.values,
      message,
    });
    copyHeaders(response.header, context.responseHeader);
    copyHeaders(response.trailer, context.responseTrailer);
    return response;
  }

  async #interceptStream(
    method: DescMethod,
    messages: AsyncIterable<MessageShape<DescMessage>>,
    context: HandlerContext,
    handler: (requests: AsyncIterable<MessageShape<DescMessage>>, context: HandlerContext) => AsyncIterable<unknown>,
  ): Promise<StreamResponse> {
    let next = async (request: StreamRequest): Promise<StreamResponse> => {
      const output = handler(request.message, handlerContextForRequest(context, request));
      const normalized = (async function* (server: Server) {
        for await (const message of output) {
          yield server.#coerceMessage(method.output, message);
        }
      })(this);
      return {
        stream: true,
        service: method.parent,
        method: method as StreamRequest["method"],
        header: context.responseHeader,
        trailer: context.responseTrailer,
        message: normalized,
      };
    };
    for (const interceptor of [...this.interceptors].reverse()) {
      next = interceptor(next as unknown as Parameters<Interceptor>[0]) as typeof next;
    }
    return next({
      stream: true,
      service: method.parent,
      method: method as StreamRequest["method"],
      requestMethod: context.requestMethod,
      url: context.url,
      signal: context.signal,
      header: context.requestHeader,
      contextValues: context.values,
      message: messages,
    });
  }

  private serviceByName(serviceName: string): Map<string, ServiceInfo> {
    const service = this.#parsed.services.get(serviceName);
    if (!service) {
      throw new Error(
        `Service '${serviceName}' not found in descriptor. Available: ${JSON.stringify([...this.#parsed.services.keys()])}`,
      );
    }
    return new Map([[serviceName, service]]);
  }

  private validateGeneratedService(service: DescService): ServiceInfo {
    const descriptorService = this.#parsed.services.get(service.typeName);
    if (!descriptorService) {
      throw new Error(`Generated service '${service.typeName}' is not present in descriptor.binpb.`);
    }
    if (!equals(ServiceDescriptorProtoSchema, service.proto, descriptorService.desc.proto)) {
      throw new Error(`Generated service '${service.typeName}' does not match descriptor.binpb.`);
    }

    const seenMessages = new Set<string>();
    const seenEnums = new Set<string>();
    for (const method of service.methods) {
      this.validateGeneratedMessage(method.input, seenMessages, seenEnums);
      this.validateGeneratedMessage(method.output, seenMessages, seenEnums);
    }
    const mismatch = descriptorGraphMismatch(
      reachableServiceFiles(service),
      reachableServiceFiles(descriptorService.desc),
    );
    if (mismatch !== undefined) {
      throw new Error(
        `Generated service '${service.typeName}' protobuf file '${mismatch}' does not match descriptor.binpb.`,
      );
    }
    return descriptorService;
  }

  private validateGeneratedMessage(message: DescMessage, seenMessages: Set<string>, seenEnums: Set<string>): void {
    if (seenMessages.has(message.typeName)) {
      return;
    }
    seenMessages.add(message.typeName);

    const descriptorMessage = this.#parsed.getMessage(message.typeName);
    if (
      !descriptorMessage ||
      !equals(DescriptorProtoSchema, message.proto, descriptorMessage.proto) ||
      !messageSemanticsMatch(message, descriptorMessage)
    ) {
      throw new Error(`Generated message '${message.typeName}' does not match descriptor.binpb.`);
    }

    for (const field of message.fields) {
      if (field.fieldKind === "message") {
        this.validateGeneratedMessage(field.message, seenMessages, seenEnums);
      } else if (field.fieldKind === "enum") {
        this.validateGeneratedEnum(field.enum, seenEnums);
      } else if (field.fieldKind === "list") {
        if (field.listKind === "message") {
          this.validateGeneratedMessage(field.message, seenMessages, seenEnums);
        } else if (field.listKind === "enum") {
          this.validateGeneratedEnum(field.enum, seenEnums);
        }
      } else if (field.fieldKind === "map") {
        if (field.mapKind === "message") {
          this.validateGeneratedMessage(field.message, seenMessages, seenEnums);
        } else if (field.mapKind === "enum") {
          this.validateGeneratedEnum(field.enum, seenEnums);
        }
      }
    }
  }

  private validateGeneratedEnum(en: DescEnum, seenEnums: Set<string>): void {
    if (seenEnums.has(en.typeName)) {
      return;
    }
    seenEnums.add(en.typeName);
    const descriptorEnum = this.#parsed.getEnum(en.typeName);
    if (
      !descriptorEnum ||
      !equals(EnumDescriptorProtoSchema, en.proto, descriptorEnum.proto) ||
      en.open !== descriptorEnum.open
    ) {
      throw new Error(`Generated enum '${en.typeName}' does not match descriptor.binpb.`);
    }
  }

  private shouldInclude(serviceFullName: string, methodName: string): boolean {
    const fullPath = `${serviceFullName}.${methodName}`;
    const includes = [...this.includes, ...splitPatterns(process.env.INVARIANT_INCLUDE ?? "")];
    const excludes = [...this.excludes, ...splitPatterns(process.env.INVARIANT_EXCLUDE ?? "")];

    if (includes.length > 0 && !includes.some((pattern) => globMatch(pattern, fullPath))) {
      return false;
    }
    return !excludes.some((pattern) => globMatch(pattern, fullPath));
  }
}

function splitPatterns(value: string): string[] {
  return value
    .split(",")
    .map((part) => part.trim())
    .filter(Boolean);
}

function globMatch(pattern: string, value: string): boolean {
  const escaped = pattern.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*");
  return new RegExp(`^${escaped}$`).test(value);
}

function isMessageFor(desc: DescMessage, value: unknown): boolean {
  return (
    typeof value === "object" &&
    value !== null &&
    "$typeName" in value &&
    (value as { $typeName?: string }).$typeName === desc.typeName
  );
}

function reservedInboundMetadata(key: string): boolean {
  const logicalKey = key.endsWith("-bin") ? key.slice(0, -4) : key;
  if (
    logicalKey.startsWith("grpc-") ||
    logicalKey.startsWith("connect-") ||
    logicalKey.startsWith("invariant-internal-") ||
    logicalKey.startsWith("x-invariant-internal-") ||
    logicalKey.startsWith("x-tenant") ||
    logicalKey.startsWith("x-principal") ||
    logicalKey.startsWith("x-role") ||
    logicalKey.startsWith("x-user") ||
    logicalKey.startsWith("x-auth") ||
    logicalKey.startsWith("x-internal-") ||
    logicalKey.startsWith("internal-") ||
    logicalKey.startsWith("tenant-") ||
    logicalKey.startsWith("principal-") ||
    logicalKey.startsWith("role-") ||
    logicalKey.startsWith("user-") ||
    logicalKey.startsWith("auth-") ||
    logicalKey.startsWith("subject-") ||
    logicalKey.startsWith("identity-")
  ) {
    return true;
  }
  return new Set([
    "authorization",
    "proxy-authorization",
    "cookie",
    "set-cookie",
    "authentication",
    "api-key",
    "x-api-key",
    "tenant",
    "principal",
    "role",
    "user",
    "subject",
    "identity",
    "te",
    "host",
    "connection",
    "keep-alive",
    "proxy-connection",
    "transfer-encoding",
    "upgrade",
    "content-length",
    "content-type",
    "trailer",
  ]).has(logicalKey);
}

function validMetadataKey(key: string): boolean {
  return key.length > 0 && /^[a-z0-9._-]+$/.test(key);
}

function validMetadataValue(value: string): boolean {
  return value.length > 0 && /^[\x20-\x7e]+$/.test(value);
}

function positiveByteLimit(value: number, subject: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new RangeError(`${subject} must be a positive integer number of bytes.`);
  }
  return value;
}

function runWithContext<T>(context: HandlerContext, operation: () => Promise<T>): Promise<T> {
  try {
    assertContextActive(context);
  } catch (error) {
    return Promise.reject(error);
  }
  return new Promise<T>((resolve, reject) => {
    const abort = () => {
      context.signal.removeEventListener("abort", abort);
      reject(context.signal.reason ?? new ConnectError("request canceled", ConnectCode.Canceled));
    };
    context.signal.addEventListener("abort", abort, { once: true });
    void Promise.resolve()
      .then(() => {
        assertContextActive(context);
        return operation();
      })
      .then(
        (value) => {
          context.signal.removeEventListener("abort", abort);
          try {
            assertContextActive(context);
            resolve(value);
          } catch (error) {
            reject(error);
          }
        },
        (error) => {
          context.signal.removeEventListener("abort", abort);
          reject(error);
        },
      );
  });
}

function assertContextActive(context: HandlerContext): void {
  if (context.signal.aborted) {
    throw context.signal.reason ?? new ConnectError("request canceled", ConnectCode.Canceled);
  }
  const remaining = context.timeoutMs();
  if (remaining !== undefined && remaining <= 0) {
    throw new ConnectError("deadline exceeded", ConnectCode.DeadlineExceeded);
  }
}

function reachableServiceFiles(service: DescService): Map<string, DescFile> {
  const files = new Map<string, DescFile>([[service.file.proto.name, service.file]]);
  const seen = new Set<string>();
  for (const method of service.methods) {
    addReachableMessageFiles(files, seen, method.input);
    addReachableMessageFiles(files, seen, method.output);
  }
  return files;
}

function addReachableMessageFiles(files: Map<string, DescFile>, seen: Set<string>, message: DescMessage): void {
  if (seen.has(message.typeName)) {
    return;
  }
  seen.add(message.typeName);
  files.set(message.file.proto.name, message.file);
  for (const field of message.fields) {
    if (field.fieldKind === "message") {
      addReachableMessageFiles(files, seen, field.message);
    } else if (field.fieldKind === "enum") {
      files.set(field.enum.file.proto.name, field.enum.file);
    } else if (field.fieldKind === "list") {
      if (field.listKind === "message") {
        addReachableMessageFiles(files, seen, field.message);
      } else if (field.listKind === "enum") {
        files.set(field.enum.file.proto.name, field.enum.file);
      }
    } else if (field.fieldKind === "map") {
      if (field.mapKind === "message") {
        addReachableMessageFiles(files, seen, field.message);
      } else if (field.mapKind === "enum") {
        files.set(field.enum.file.proto.name, field.enum.file);
      }
    }
  }
}

function descriptorGraphMismatch(generated: Map<string, DescFile>, runtime: Map<string, DescFile>): string | undefined {
  if (generated.size !== runtime.size) {
    return "<reachable graph>";
  }
  for (const path of [...generated.keys()].sort()) {
    const generatedFile = generated.get(path);
    const runtimeFile = runtime.get(path);
    if (!generatedFile || !runtimeFile) {
      return path;
    }
    if (generatedFile.edition !== runtimeFile.edition || generatedFile.deprecated !== runtimeFile.deprecated) {
      return path;
    }
    const generatedProto = clone(FileDescriptorProtoSchema, generatedFile.proto);
    const runtimeProto = clone(FileDescriptorProtoSchema, runtimeFile.proto);
    generatedProto.sourceCodeInfo = undefined;
    runtimeProto.sourceCodeInfo = undefined;
    // Protobuf-ES deliberately trims language-specific file options from its
    // embedded generated descriptors. Resolved wire semantics are checked on
    // DescFile, DescMessage, DescField, and DescEnum instead.
    generatedProto.options = undefined;
    runtimeProto.options = undefined;
    if (!equals(FileDescriptorProtoSchema, generatedProto, runtimeProto)) {
      return path;
    }
  }
  return undefined;
}

function messageSemanticsMatch(generated: DescMessage, runtime: DescMessage): boolean {
  for (const field of generated.fields) {
    const expected = runtime.fields.find((candidate) => candidate.name === field.name);
    if (
      !expected ||
      field.fieldKind !== expected.fieldKind ||
      field.presence !== expected.presence ||
      field.utf8Validation !== expected.utf8Validation
    ) {
      return false;
    }
    if (field.fieldKind === "scalar" && expected.fieldKind === "scalar") {
      if (field.scalar !== expected.scalar || field.longAsString !== expected.longAsString) {
        return false;
      }
    } else if (field.fieldKind === "message" && expected.fieldKind === "message") {
      if (field.delimitedEncoding !== expected.delimitedEncoding) {
        return false;
      }
    } else if (field.fieldKind === "list" && expected.fieldKind === "list") {
      if (field.listKind !== expected.listKind || field.packed !== expected.packed) {
        return false;
      }
      if (
        field.listKind === "scalar" &&
        expected.listKind === "scalar" &&
        (field.scalar !== expected.scalar || field.longAsString !== expected.longAsString)
      ) {
        return false;
      }
      if (
        field.listKind === "message" &&
        expected.listKind === "message" &&
        field.delimitedEncoding !== expected.delimitedEncoding
      ) {
        return false;
      }
    } else if (
      field.fieldKind === "map" &&
      expected.fieldKind === "map" &&
      (field.mapKind !== expected.mapKind || field.delimitedEncoding !== expected.delimitedEncoding)
    ) {
      return false;
    }
  }
  return true;
}

function publicTool(tool: RegisteredTool): Tool {
  return deepFreeze({
    name: tool.name,
    description: tool.description,
    inputSchema: structuredClone(tool.inputSchema),
    inputType: tool.inputType,
    outputType: tool.outputType,
    serviceFullName: tool.serviceFullName,
    methodName: tool.methodName,
    serverStreaming: tool.serverStreaming,
  }) as Tool;
}

function deepFreeze<T>(value: T): Readonly<T> {
  if (typeof value !== "object" || value === null || Object.isFrozen(value)) {
    return value;
  }
  for (const nested of Object.values(value as Record<string, unknown>)) {
    deepFreeze(nested);
  }
  return Object.freeze(value);
}

function readOnlyMap<K, V>(source: Map<K, V>): ReadonlyMap<K, V> {
  return new Proxy(source, {
    get(target, property) {
      if (property === "set" || property === "delete" || property === "clear") {
        return () => {
          throw new TypeError("Invariant's registered tool catalog is read-only.");
        };
      }
      const value = Reflect.get(target, property, target) as unknown;
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
}

function handlerContextForRequest(context: HandlerContext, request: UnaryRequest | StreamRequest): HandlerContext {
  return Object.assign({}, context, {
    service: request.service,
    method: request.method,
    requestMethod: request.requestMethod,
    url: request.url,
    requestHeader: request.header,
    signal: request.signal,
    values: request.contextValues,
  });
}

async function* oneMessage(message: MessageShape<DescMessage>): AsyncIterable<MessageShape<DescMessage>> {
  yield message;
}

async function exactlyOneMessage(
  messages: AsyncIterable<MessageShape<DescMessage>>,
  subject: string,
): Promise<MessageShape<DescMessage>> {
  const iterator = messages[Symbol.asyncIterator]();
  const first = await iterator.next();
  if (first.done) {
    throw new InvariantError("internal", `${subject} produced no message where exactly one was required`);
  }
  const second = await iterator.next();
  if (!second.done) {
    throw new InvariantError("internal", `${subject} produced multiple messages where exactly one was required`);
  }
  return first.value;
}

function copyHeaders(source: Headers, target: Headers): void {
  if (source === target) {
    return;
  }
  target.forEach((_value, key) => {
    target.delete(key);
  });
  source.forEach((value, key) => {
    target.set(key, value);
  });
}
