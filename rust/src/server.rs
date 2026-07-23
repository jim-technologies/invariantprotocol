//! Canonical generated-tonic registration plus shared typed dispatch.

use crate::descriptor::ParsedDescriptor;
use crate::schema::SchemaGen;
use futures::FutureExt;
use futures::future::BoxFuture;
use futures::stream::{BoxStream, StreamExt};
use parking_lot::RwLock;
use prost::{Message, Name};
use prost_reflect::{DynamicMessage, MessageDescriptor};
use serde_json::{Value, json};
use std::any::Any;
use std::collections::{BTreeMap, BTreeSet};
use std::convert::Infallible;
use std::panic::AssertUnwindSafe;
use std::sync::Arc;
use std::time::Duration;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;
use tonic::body::Body;
use tonic::metadata::{KeyAndValueRef, MetadataMap};
use tonic::server::NamedService;
use tonic::service::Routes;
use tonic::{Request, Response, Status};
use tower::Service;

pub const SERVER_NAME: &str = "invariant-protocol";
pub const SERVER_VERSION: &str = env!("CARGO_PKG_VERSION");

pub type BoxResponseStream<T> =
    std::pin::Pin<Box<dyn futures::Stream<Item = Result<T, Status>> + Send + 'static>>;
pub type DynamicResponseStream = BoxStream<'static, Result<DynamicMessage, Status>>;

#[doc(hidden)]
pub trait NativeService: Clone + Send + Sync + 'static {
    const NAME: &'static str;

    fn add_to_routes(self, routes: Routes) -> Routes;
}

impl<S> NativeService for S
where
    S: Service<http::Request<Body>, Error = Infallible>
        + NamedService
        + Clone
        + Send
        + Sync
        + 'static,
    S::Response: axum::response::IntoResponse,
    S::Future: Send + 'static,
{
    const NAME: &'static str = S::NAME;

    fn add_to_routes(self, routes: Routes) -> Routes {
        routes.add_service(self)
    }
}

pub const DEFAULT_MAX_UNARY_REQUEST_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_UNARY_RESPONSE_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_STREAM_REQUEST_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_STREAM_RESPONSE_BYTES: usize = 16 * 1024 * 1024;

/// Per-method HTTP/Connect encoded-message limits. A zero field inherits the
/// corresponding server default. Native gRPC keeps Tonic's independent
/// protobuf message-size configuration.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct MethodConfig {
    pub max_unary_request_bytes: usize,
    pub max_unary_response_bytes: usize,
    pub max_stream_request_bytes: usize,
    pub max_stream_response_bytes: usize,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct HTTPMethodLimits {
    pub unary_request: usize,
    pub unary_response: usize,
    pub stream_request: usize,
    pub stream_response: usize,
}

/// Selects untrusted HTTP headers that may become incoming gRPC metadata.
/// Invariant always removes identity, authorization, protocol, and internal
/// keys after this function returns.
pub type HTTPMetadataMapper = Arc<dyn Fn(&http::HeaderMap) -> MetadataMap + Send + Sync>;

/// The projection deadline and cancellation signal placed in every HTTP-backed
/// Tonic request extension. Dropping a disconnected request or response stream
/// cancels the signal; `Connect-Timeout-Ms` additionally exposes a deadline.
#[derive(Clone, Debug)]
pub struct ProjectionContext {
    deadline: Option<Instant>,
    cancellation: CancellationToken,
}

impl ProjectionContext {
    pub(crate) fn new(timeout: Option<Duration>) -> Self {
        Self {
            deadline: timeout.map(|timeout| Instant::now() + timeout),
            cancellation: CancellationToken::new(),
        }
    }

    pub fn deadline(&self) -> Option<Instant> {
        self.deadline
    }

    pub fn remaining(&self) -> Option<Duration> {
        self.deadline
            .map(|deadline| deadline.saturating_duration_since(Instant::now()))
    }

    pub fn is_cancelled(&self) -> bool {
        self.cancellation.is_cancelled()
    }

    pub async fn cancelled(&self) {
        self.cancellation.cancelled().await;
    }

    pub(crate) fn cancel(&self) {
        self.cancellation.cancel();
    }
}

pub(crate) struct ProjectionGuard(ProjectionContext);

impl ProjectionGuard {
    pub(crate) fn new(context: ProjectionContext) -> Self {
        Self(context)
    }
}

impl Drop for ProjectionGuard {
    fn drop(&mut self) {
        self.0.cancel();
    }
}

#[derive(Debug, Clone)]
pub struct ServerCallInfo {
    pub full_method: String,
}

impl ServerCallInfo {
    pub fn new(service: &str, method: &str) -> Self {
        Self {
            full_method: format!("/{service}/{method}"),
        }
    }
}

/// A heterogeneous container whose payload remains the complete concrete
/// generated `tonic::Request<T>`. Cross-service shared middleware must
/// downcast it because Rust cannot store `Request<A>` and `Request<B>` in one
/// standard Tower service collection. This is not a Tonic `Interceptor` or a
/// Tower `Layer`; use those standard APIs on the native generated service when
/// metadata/transport middleware is sufficient.
pub struct ErasedRequest {
    inner: Box<dyn Any + Send>,
    rust_type: &'static str,
}

impl ErasedRequest {
    fn new<T: Send + 'static>(request: Request<T>) -> Self {
        Self {
            inner: Box::new(request),
            rust_type: std::any::type_name::<T>(),
        }
    }

    pub fn downcast_ref<T: Send + 'static>(&self) -> Option<&Request<T>> {
        self.inner.downcast_ref()
    }

    pub fn downcast_mut<T: Send + 'static>(&mut self) -> Option<&mut Request<T>> {
        self.inner.downcast_mut()
    }

    pub fn rust_type(&self) -> &'static str {
        self.rust_type
    }

    fn into_typed<T: Send + 'static>(self) -> Result<Request<T>, Status> {
        self.inner
            .downcast::<Request<T>>()
            .map(|value| *value)
            .map_err(|_| {
                Status::internal(format!(
                    "interceptor changed request type; expected {}",
                    std::any::type_name::<T>()
                ))
            })
    }
}

pub struct ErasedResponse {
    inner: Box<dyn Any + Send>,
    rust_type: &'static str,
}

impl ErasedResponse {
    fn new<T: Send + 'static>(response: Response<T>) -> Self {
        Self {
            inner: Box::new(response),
            rust_type: std::any::type_name::<T>(),
        }
    }

    pub fn downcast_ref<T: Send + 'static>(&self) -> Option<&Response<T>> {
        self.inner.downcast_ref()
    }

    pub fn downcast_mut<T: Send + 'static>(&mut self) -> Option<&mut Response<T>> {
        self.inner.downcast_mut()
    }

    pub fn rust_type(&self) -> &'static str {
        self.rust_type
    }

    fn into_typed<T: Send + 'static>(self) -> Result<Response<T>, Status> {
        self.inner
            .downcast::<Response<T>>()
            .map(|value| *value)
            .map_err(|_| {
                Status::internal(format!(
                    "interceptor changed response type; expected {}",
                    std::any::type_name::<T>()
                ))
            })
    }
}

pub type SharedHandler =
    Arc<dyn Fn(ErasedRequest) -> BoxFuture<'static, Result<ErasedResponse, Status>> + Send + Sync>;
/// The minimal cross-projection adapter for middleware that must inspect
/// concrete protobuf messages. Standard Tonic interceptors are preferable for
/// native-only metadata checks and can be applied in the generated registration
/// configuration closure.
pub type SharedUnaryMiddleware = Arc<
    dyn Fn(
            ErasedRequest,
            ServerCallInfo,
            SharedHandler,
        ) -> BoxFuture<'static, Result<ErasedResponse, Status>>
        + Send
        + Sync,
>;
pub type SharedStreamMiddleware = SharedUnaryMiddleware;

pub(crate) type UnaryProjectionHandler = Arc<
    dyn Fn(Request<DynamicMessage>) -> BoxFuture<'static, Result<Response<DynamicMessage>, Status>>
        + Send
        + Sync,
>;
pub(crate) type StreamProjectionHandler = Arc<
    dyn Fn(
            Request<DynamicMessage>,
        ) -> BoxFuture<'static, Result<Response<DynamicResponseStream>, Status>>
        + Send
        + Sync,
>;

pub struct Tool {
    pub name: String,
    pub description: String,
    pub input_schema: Value,
    pub input_type: String,
    pub output_type: String,
    pub service_full_name: String,
    pub method_name: String,
    pub server_streaming: bool,
    pub(crate) input_desc: MessageDescriptor,
    pub(crate) unary: Option<UnaryProjectionHandler>,
    pub(crate) stream: Option<StreamProjectionHandler>,
}

enum PendingBinding {
    Unary {
        method: String,
        input: String,
        output: String,
        factory: Box<dyn Fn(MessageDescriptor) -> UnaryProjectionHandler + Send + Sync>,
    },
    ServerStreaming {
        method: String,
        input: String,
        output: String,
        factory: Box<dyn Fn(MessageDescriptor) -> StreamProjectionHandler + Send + Sync>,
    },
}

struct RemoteServiceRegistration {
    service_name: String,
    tools: Vec<Tool>,
}

/// Projection bindings emitted beside a normal generated tonic service.
/// Applications normally receive this through generated `register_*` helpers.
pub struct ServiceRegistration {
    service_name: String,
    descriptor_graph: Vec<u8>,
    bindings: Vec<PendingBinding>,
}

impl ServiceRegistration {
    #[doc(hidden)]
    pub fn new(service_name: &str, descriptor_graph: &[u8]) -> Self {
        Self {
            service_name: service_name.to_string(),
            descriptor_graph: descriptor_graph.to_vec(),
            bindings: Vec::new(),
        }
    }

    #[doc(hidden)]
    pub fn unary<Req, Resp, F, Fut>(&mut self, method: &str, call: F) -> Result<(), Status>
    where
        Req: Message + Name + Default + Send + 'static,
        Resp: Message + Name + Send + 'static,
        F: Fn(Request<Req>) -> Fut + Clone + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<Resp>, Status>> + Send + 'static,
    {
        let call = Arc::new(call);
        self.bindings.push(PendingBinding::Unary {
            method: method.to_string(),
            input: Req::full_name(),
            output: Resp::full_name(),
            factory: Box::new(move |output_desc| {
                let call = call.clone();
                Arc::new(move |request| {
                    let call = call.clone();
                    let output_desc = output_desc.clone();
                    Box::pin(async move {
                        let (metadata, extensions, request) = request.into_parts();
                        let typed =
                            Req::decode(request.encode_to_vec().as_slice()).map_err(|error| {
                                Status::invalid_argument(format!("decode request: {error}"))
                            })?;
                        let response =
                            call(Request::from_parts(metadata, extensions, typed)).await?;
                        let (metadata, response, extensions) = response.into_parts();
                        let dynamic = DynamicMessage::decode(
                            output_desc,
                            response.encode_to_vec().as_slice(),
                        )
                        .map_err(|error| Status::internal(format!("encode response: {error}")))?;
                        Ok(Response::from_parts(metadata, dynamic, extensions))
                    })
                })
            }),
        });
        Ok(())
    }

    #[doc(hidden)]
    pub fn server_streaming<Req, Resp, F, Fut>(
        &mut self,
        method: &str,
        call: F,
    ) -> Result<(), Status>
    where
        Req: Message + Name + Default + Send + 'static,
        Resp: Message + Name + Send + 'static,
        F: Fn(Request<Req>) -> Fut + Clone + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<BoxResponseStream<Resp>>, Status>>
            + Send
            + 'static,
    {
        let call = Arc::new(call);
        self.bindings.push(PendingBinding::ServerStreaming {
            method: method.to_string(),
            input: Req::full_name(),
            output: Resp::full_name(),
            factory: Box::new(move |output_desc| {
                let call = call.clone();
                Arc::new(move |request| {
                    let call = call.clone();
                    let output_desc = output_desc.clone();
                    Box::pin(async move {
                        let (metadata, extensions, request) = request.into_parts();
                        let typed =
                            Req::decode(request.encode_to_vec().as_slice()).map_err(|error| {
                                Status::invalid_argument(format!("decode request: {error}"))
                            })?;
                        let response =
                            call(Request::from_parts(metadata, extensions, typed)).await?;
                        let (metadata, stream, extensions) = response.into_parts();
                        let mapped = stream.map(move |item| {
                            let output_desc = output_desc.clone();
                            item.and_then(|message| {
                                DynamicMessage::decode(
                                    output_desc,
                                    message.encode_to_vec().as_slice(),
                                )
                                .map_err(|error| {
                                    Status::internal(format!("encode stream response: {error}"))
                                })
                            })
                        });
                        let mapped: DynamicResponseStream = Box::pin(mapped);
                        Ok(Response::from_parts(metadata, mapped, extensions))
                    })
                })
            }),
        });
        Ok(())
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Phase {
    Configuring,
    Frozen,
}

struct Registry {
    phase: Phase,
    tools: BTreeMap<String, Arc<Tool>>,
    services: BTreeSet<String>,
    reflected_methods: BTreeMap<String, BTreeSet<String>>,
    native_routes: Routes,
    shared_unary_middleware: Vec<SharedUnaryMiddleware>,
    shared_stream_middleware: Vec<SharedStreamMiddleware>,
    includes: Vec<String>,
    excludes: Vec<String>,
    http_metadata_mapper: HTTPMetadataMapper,
    http_max_unary_request: usize,
    http_max_unary_response: usize,
    connect_stream_max_request: usize,
    connect_stream_max_response: usize,
    method_configs: BTreeMap<String, MethodConfig>,
}

struct ServerInner {
    parsed: ParsedDescriptor,
    registry: RwLock<Registry>,
}

/// Cloneable handle to one immutable descriptor graph and its registrations.
#[derive(Clone)]
pub struct Server {
    inner: Arc<ServerInner>,
}

impl Server {
    pub fn from_descriptor(path: &str) -> Result<Self, Status> {
        Ok(Self::with_parsed(ParsedDescriptor::from_file(path)?))
    }

    pub fn from_bytes(data: &[u8]) -> Result<Self, Status> {
        Ok(Self::with_parsed(ParsedDescriptor::from_bytes(data)?))
    }

    fn with_parsed(parsed: ParsedDescriptor) -> Self {
        let includes = environment_patterns("INVARIANT_INCLUDE");
        let excludes = environment_patterns("INVARIANT_EXCLUDE");
        Self {
            inner: Arc::new(ServerInner {
                parsed,
                registry: RwLock::new(Registry {
                    phase: Phase::Configuring,
                    tools: BTreeMap::new(),
                    services: BTreeSet::new(),
                    reflected_methods: BTreeMap::new(),
                    native_routes: Routes::default(),
                    shared_unary_middleware: Vec::new(),
                    shared_stream_middleware: Vec::new(),
                    includes,
                    excludes,
                    http_metadata_mapper: Arc::new(default_http_metadata_mapper),
                    http_max_unary_request: DEFAULT_MAX_UNARY_REQUEST_BYTES,
                    http_max_unary_response: DEFAULT_MAX_UNARY_RESPONSE_BYTES,
                    connect_stream_max_request: DEFAULT_MAX_STREAM_REQUEST_BYTES,
                    connect_stream_max_response: DEFAULT_MAX_STREAM_RESPONSE_BYTES,
                    method_configs: BTreeMap::new(),
                }),
            }),
        }
    }

    #[doc(hidden)]
    pub fn register_generated_service<S>(
        &self,
        native: S,
        registration: ServiceRegistration,
    ) -> Result<(), Status>
    where
        S: NativeService,
    {
        if S::NAME != registration.service_name {
            return Err(Status::invalid_argument(format!(
                "generated native service {} disagrees with registration {}",
                S::NAME,
                registration.service_name
            )));
        }
        let service = self.inner.parsed.services.get(S::NAME).ok_or_else(|| {
            Status::not_found(format!(
                "service {} is absent from descriptor.binpb",
                S::NAME
            ))
        })?;
        let actual_graph = self.inner.parsed.service_graph(S::NAME)?;
        if actual_graph != registration.descriptor_graph {
            return Err(Status::failed_precondition(format!(
                "generated service {} descriptor graph disagrees with descriptor.binpb",
                S::NAME
            )));
        }

        let expected = service
            .methods
            .values()
            .filter(|method| !method.client_streaming)
            .map(|method| method.name.clone())
            .collect::<BTreeSet<_>>();
        let actual = registration
            .bindings
            .iter()
            .map(|binding| match binding {
                PendingBinding::Unary { method, .. }
                | PendingBinding::ServerStreaming { method, .. } => method.clone(),
            })
            .collect::<BTreeSet<_>>();
        if expected != actual || actual.len() != registration.bindings.len() {
            return Err(Status::failed_precondition(format!(
                "generated projection methods for {} disagree with descriptor.binpb",
                S::NAME
            )));
        }

        let mut tools = Vec::with_capacity(registration.bindings.len());
        for binding in registration.bindings {
            let (method_name, input, output, server_streaming, unary, stream) = match binding {
                PendingBinding::Unary {
                    method,
                    input,
                    output,
                    factory,
                } => (method, input, output, false, Some(factory), None),
                PendingBinding::ServerStreaming {
                    method,
                    input,
                    output,
                    factory,
                } => (method, input, output, true, None, Some(factory)),
            };
            let method = service.methods.get(&method_name).ok_or_else(|| {
                Status::failed_precondition(format!("unknown generated method {method_name}"))
            })?;
            if method.input_type != input
                || method.output_type != output
                || method.server_streaming != server_streaming
                || method.client_streaming
            {
                return Err(Status::failed_precondition(format!(
                    "/{}/{} generated types or cardinality disagree with descriptor.binpb",
                    S::NAME,
                    method_name
                )));
            }
            let input_desc = self
                .inner
                .parsed
                .pool
                .get_message_by_name(&input)
                .ok_or_else(|| Status::internal(format!("missing input descriptor {input}")))?;
            let output_desc = self
                .inner
                .parsed
                .pool
                .get_message_by_name(&output)
                .ok_or_else(|| Status::internal(format!("missing output descriptor {output}")))?;
            let tool_name = format!("{}.{method_name}", S::NAME);
            tools.push(Tool {
                name: tool_name.clone(),
                description: if method.comment.is_empty() {
                    tool_name
                } else {
                    method.comment.clone()
                },
                input_schema: SchemaGen::new(&self.inner.parsed).message_to_schema(&input),
                input_type: input,
                output_type: output,
                service_full_name: S::NAME.to_string(),
                method_name,
                server_streaming,
                input_desc,
                unary: unary.map(|factory| factory(output_desc.clone())),
                stream: stream.map(|factory| factory(output_desc)),
            });
        }

        let reflected_methods = service.methods.keys().cloned().collect();
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "service registration")?;
        if registry.services.contains(S::NAME) {
            return Err(Status::already_exists(format!(
                "service {} is already registered",
                S::NAME
            )));
        }
        for tool in &tools {
            if should_project(&registry, &tool.service_full_name, &tool.method_name)
                && registry.tools.contains_key(&tool.name)
            {
                return Err(Status::already_exists(format!(
                    "tool {} is already registered",
                    tool.name
                )));
            }
        }
        registry.native_routes = native.add_to_routes(std::mem::take(&mut registry.native_routes));
        registry.services.insert(S::NAME.to_string());
        registry
            .reflected_methods
            .insert(S::NAME.to_string(), reflected_methods);
        for tool in tools {
            if should_project(&registry, &tool.service_full_name, &tool.method_name) {
                registry.tools.insert(tool.name.clone(), Arc::new(tool));
            }
        }
        Ok(())
    }

    /// Register descriptor-declared unary methods as projections of a
    /// caller-owned Tonic service such as `Channel`, `InterceptedService`, or a
    /// Tower-wrapped transport. The configuration closure receives Tonic's
    /// normal dynamic `Grpc` client so compression and message limits remain
    /// ordinary Tonic call controls. Streaming methods are intentionally not
    /// proxied.
    pub fn connect_grpc<T, F>(&self, service: T, configure: F) -> Result<(), Status>
    where
        T: tonic::client::GrpcService<Body> + Clone + Send + Sync + 'static,
        T::ResponseBody: tonic::codegen::Body<Data = tonic::codegen::Bytes> + Send + 'static,
        <T::ResponseBody as tonic::codegen::Body>::Error: Into<tonic::codegen::StdError> + Send,
        T::Error: Into<tonic::codegen::StdError>,
        T::Future: Send + 'static,
        F: Fn(tonic::client::Grpc<T>) -> tonic::client::Grpc<T> + Clone + Send + Sync + 'static,
    {
        {
            let registry = self.inner.registry.read();
            ensure_configuring(&registry, "remote gRPC registration")?;
        }
        let mut registrations = Vec::new();
        for (service_name, service_info) in &self.inner.parsed.services {
            let mut tools = Vec::new();
            for method in service_info.methods.values() {
                if method.client_streaming || method.server_streaming {
                    continue;
                }
                let input = self
                    .inner
                    .parsed
                    .pool
                    .get_message_by_name(&method.input_type)
                    .ok_or_else(|| {
                        Status::internal(format!("missing input descriptor {}", method.input_type))
                    })?;
                let output = self
                    .inner
                    .parsed
                    .pool
                    .get_message_by_name(&method.output_type)
                    .ok_or_else(|| {
                        Status::internal(format!(
                            "missing output descriptor {}",
                            method.output_type
                        ))
                    })?;
                let full_method = format!("/{service_name}/{}", method.name)
                    .parse::<http::uri::PathAndQuery>()
                    .map_err(|error| {
                        Status::internal(format!("invalid generated method path: {error}"))
                    })?;
                let upstream = crate::projections::grpc::remote_unary_handler(
                    service.clone(),
                    configure.clone(),
                    full_method,
                    output,
                );
                let handler = self.shared_remote_unary_handler(
                    ServerCallInfo::new(service_name, &method.name),
                    upstream,
                );
                let tool_name = format!("{service_name}.{}", method.name);
                tools.push(Tool {
                    name: tool_name.clone(),
                    description: if method.comment.is_empty() {
                        tool_name
                    } else {
                        method.comment.clone()
                    },
                    input_schema: SchemaGen::new(&self.inner.parsed)
                        .message_to_schema(&method.input_type),
                    input_type: method.input_type.clone(),
                    output_type: method.output_type.clone(),
                    service_full_name: service_name.clone(),
                    method_name: method.name.clone(),
                    server_streaming: false,
                    input_desc: input,
                    unary: Some(handler),
                    stream: None,
                });
            }
            if !tools.is_empty() {
                registrations.push(RemoteServiceRegistration {
                    service_name: service_name.clone(),
                    tools,
                });
            }
        }
        self.commit_remote_services(registrations, "remote gRPC registration")
    }

    /// Register descriptor-declared unary methods against a caller-owned
    /// Reqwest client and HTTP(S) base URL. The caller's client configuration
    /// owns TLS, proxies, pools, and credentials; Invariant creates no hidden
    /// transport. Calls use canonical Connect method paths and protobuf bodies.
    pub fn connect_http(
        &self,
        client: &reqwest::Client,
        base_url: reqwest::Url,
    ) -> Result<(), Status> {
        {
            let registry = self.inner.registry.read();
            ensure_configuring(&registry, "remote HTTP registration")?;
        }
        if !matches!(base_url.scheme(), "http" | "https")
            || base_url.host_str().is_none()
            || base_url.cannot_be_a_base()
        {
            return Err(Status::invalid_argument(
                "remote HTTP base URL must be an absolute http:// or https:// URL",
            ));
        }

        let mut registrations = Vec::new();
        for (service_name, service_info) in &self.inner.parsed.services {
            let mut tools = Vec::new();
            for method in service_info.methods.values() {
                if method.client_streaming || method.server_streaming {
                    continue;
                }
                let input = self
                    .inner
                    .parsed
                    .pool
                    .get_message_by_name(&method.input_type)
                    .ok_or_else(|| {
                        Status::internal(format!("missing input descriptor {}", method.input_type))
                    })?;
                let output = self
                    .inner
                    .parsed
                    .pool
                    .get_message_by_name(&method.output_type)
                    .ok_or_else(|| {
                        Status::internal(format!(
                            "missing output descriptor {}",
                            method.output_type
                        ))
                    })?;
                let upstream = crate::projections::http_client::remote_unary_handler(
                    self.clone(),
                    client.clone(),
                    base_url.clone(),
                    service_name.clone(),
                    method.name.clone(),
                    output,
                );
                let handler = self.shared_remote_unary_handler(
                    ServerCallInfo::new(service_name, &method.name),
                    upstream,
                );
                let tool_name = format!("{service_name}.{}", method.name);
                tools.push(Tool {
                    name: tool_name.clone(),
                    description: if method.comment.is_empty() {
                        tool_name
                    } else {
                        method.comment.clone()
                    },
                    input_schema: SchemaGen::new(&self.inner.parsed)
                        .message_to_schema(&method.input_type),
                    input_type: method.input_type.clone(),
                    output_type: method.output_type.clone(),
                    service_full_name: service_name.clone(),
                    method_name: method.name.clone(),
                    server_streaming: false,
                    input_desc: input,
                    unary: Some(handler),
                    stream: None,
                });
            }
            if !tools.is_empty() {
                registrations.push(RemoteServiceRegistration {
                    service_name: service_name.clone(),
                    tools,
                });
            }
        }
        self.commit_remote_services(registrations, "remote HTTP registration")
    }

    fn shared_remote_unary_handler(
        &self,
        info: ServerCallInfo,
        upstream: UnaryProjectionHandler,
    ) -> UnaryProjectionHandler {
        let server = self.clone();
        Arc::new(move |request| {
            let server = server.clone();
            let info = info.clone();
            let upstream = upstream.clone();
            Box::pin(async move {
                server
                    .run_typed(
                        request,
                        info,
                        move |request| {
                            let upstream = upstream.clone();
                            async move { upstream(request).await }
                        },
                        false,
                    )
                    .await
            })
        })
    }

    fn commit_remote_services(
        &self,
        registrations: Vec<RemoteServiceRegistration>,
        operation: &str,
    ) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, operation)?;
        let mut staged_tools = BTreeSet::new();
        for registration in &registrations {
            if registry.services.contains(&registration.service_name) {
                return Err(Status::already_exists(format!(
                    "service {} is already registered",
                    registration.service_name
                )));
            }
            for tool in &registration.tools {
                if should_project(&registry, &tool.service_full_name, &tool.method_name)
                    && (registry.tools.contains_key(&tool.name)
                        || !staged_tools.insert(tool.name.clone()))
                {
                    return Err(Status::already_exists(format!(
                        "tool {} is already registered",
                        tool.name
                    )));
                }
            }
        }

        let mut router = std::mem::take(&mut registry.native_routes).into_axum_router();
        for registration in registrations {
            let RemoteServiceRegistration {
                service_name,
                tools,
            } = registration;
            let reflected_methods = tools.iter().map(|tool| tool.method_name.clone()).collect();
            for tool in tools {
                let full_method = format!("/{}/{}", tool.service_full_name, tool.method_name);
                let handler = tool.unary.clone().expect("remote unary handler");
                let input = tool.input_desc.clone();
                let service = tower::service_fn(move |request| {
                    let handler = handler.clone();
                    let input = input.clone();
                    async move {
                        Ok::<_, Infallible>(
                            crate::projections::grpc::serve_remote_unary(handler, input, request)
                                .await,
                        )
                    }
                });
                router = router.route_service(&full_method, service);
                if should_project(&registry, &tool.service_full_name, &tool.method_name) {
                    registry.tools.insert(tool.name.clone(), Arc::new(tool));
                }
            }
            registry
                .reflected_methods
                .insert(service_name.clone(), reflected_methods);
            registry.services.insert(service_name);
        }
        registry.native_routes = Routes::from(router);
        Ok(())
    }

    pub fn use_shared_unary(&self, middleware: SharedUnaryMiddleware) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "shared unary middleware")?;
        registry.shared_unary_middleware.push(middleware);
        Ok(())
    }

    pub fn use_shared_stream(&self, middleware: SharedStreamMiddleware) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "shared stream middleware")?;
        registry.shared_stream_middleware.push(middleware);
        Ok(())
    }

    /// Include optional projection methods matching `pattern`. `*` matches any
    /// sequence, including dots. Native generated Tonic routes are never
    /// filtered.
    pub fn include(&self, pattern: impl Into<String>) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "include filters")?;
        ensure_no_services(&registry, "include filters")?;
        registry.includes.push(pattern.into());
        Ok(())
    }

    /// Exclude optional projection methods matching `pattern`, after includes
    /// are evaluated. Native generated Tonic routes are never filtered.
    pub fn exclude(&self, pattern: impl Into<String>) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "exclude filters")?;
        ensure_no_services(&registry, "exclude filters")?;
        registry.excludes.push(pattern.into());
        Ok(())
    }

    pub fn use_http_metadata_mapper(&self, mapper: HTTPMetadataMapper) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "HTTP metadata mapper")?;
        registry.http_metadata_mapper = mapper;
        Ok(())
    }

    pub fn set_max_unary_request_bytes(&self, value: usize) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "HTTP unary request limit")?;
        registry.http_max_unary_request = if value == 0 {
            DEFAULT_MAX_UNARY_REQUEST_BYTES
        } else {
            value
        };
        Ok(())
    }

    pub fn set_max_unary_response_bytes(&self, value: usize) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "HTTP unary response limit")?;
        registry.http_max_unary_response = if value == 0 {
            DEFAULT_MAX_UNARY_RESPONSE_BYTES
        } else {
            value
        };
        Ok(())
    }

    pub fn set_max_stream_request_bytes(&self, value: usize) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "HTTP stream request limit")?;
        registry.connect_stream_max_request = if value == 0 {
            DEFAULT_MAX_STREAM_REQUEST_BYTES
        } else {
            value
        };
        Ok(())
    }

    pub fn set_max_stream_response_bytes(&self, value: usize) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "HTTP stream response limit")?;
        registry.connect_stream_max_response = if value == 0 {
            DEFAULT_MAX_STREAM_RESPONSE_BYTES
        } else {
            value
        };
        Ok(())
    }

    /// Override the four encoded HTTP limits for one canonical full method
    /// path, for example `/greet.v1.GreetService/Greet`.
    pub fn configure_method(
        &self,
        full_method: impl Into<String>,
        config: MethodConfig,
    ) -> Result<(), Status> {
        let mut registry = self.inner.registry.write();
        ensure_configuring(&registry, "method configuration")?;
        registry.method_configs.insert(full_method.into(), config);
        Ok(())
    }

    pub fn max_unary_request_bytes(&self) -> usize {
        self.inner.registry.read().http_max_unary_request
    }

    pub fn max_unary_response_bytes(&self) -> usize {
        self.inner.registry.read().http_max_unary_response
    }

    pub fn max_stream_request_bytes(&self) -> usize {
        self.inner.registry.read().connect_stream_max_request
    }

    pub fn max_stream_response_bytes(&self) -> usize {
        self.inner.registry.read().connect_stream_max_response
    }

    pub(crate) fn http_limits(&self, tool: &Tool) -> HTTPMethodLimits {
        self.http_limits_for_method(&tool.service_full_name, &tool.method_name)
    }

    pub(crate) fn http_limits_for_method(&self, service: &str, method: &str) -> HTTPMethodLimits {
        let registry = self.inner.registry.read();
        let full_method = format!("/{service}/{method}");
        let config = registry
            .method_configs
            .get(&full_method)
            .copied()
            .unwrap_or_default();
        HTTPMethodLimits {
            unary_request: nonzero_or(
                config.max_unary_request_bytes,
                registry.http_max_unary_request,
            ),
            unary_response: nonzero_or(
                config.max_unary_response_bytes,
                registry.http_max_unary_response,
            ),
            stream_request: nonzero_or(
                config.max_stream_request_bytes,
                registry.connect_stream_max_request,
            ),
            stream_response: nonzero_or(
                config.max_stream_response_bytes,
                registry.connect_stream_max_response,
            ),
        }
    }

    pub(crate) fn incoming_http_metadata(&self, headers: &http::HeaderMap) -> MetadataMap {
        let mapper = self.inner.registry.read().http_metadata_mapper.clone();
        filter_incoming_metadata((mapper)(headers))
    }

    pub fn freeze(&self) {
        self.inner.registry.write().phase = Phase::Frozen;
    }

    /// Extract the registered native Tonic routes and freeze all registration
    /// and projection configuration. The caller owns listener and graceful
    /// lifecycle policy through normal `tonic::transport::Server` methods such
    /// as `serve_with_shutdown` or `serve_with_incoming_shutdown`.
    pub fn grpc_routes(&self) -> tonic::service::Routes {
        self.freeze();
        let routes = self.inner.registry.read().native_routes.clone();
        let reflection = crate::projections::grpc::build_reflection(self)
            .expect("reflection service from validated descriptor image");
        routes.add_service(reflection).prepare()
    }

    pub fn tool(&self, name: &str) -> Option<Arc<Tool>> {
        self.inner.registry.read().tools.get(name).cloned()
    }

    pub fn tools_snapshot(&self) -> Vec<Arc<Tool>> {
        self.inner.registry.read().tools.values().cloned().collect()
    }

    pub fn parsed(&self) -> &ParsedDescriptor {
        &self.inner.parsed
    }

    pub(crate) fn reflected_service_methods(&self) -> BTreeMap<String, BTreeSet<String>> {
        self.inner.registry.read().reflected_methods.clone()
    }

    pub fn tool_catalog(&self) -> Vec<Value> {
        self.tools_snapshot()
            .into_iter()
            .map(|tool| {
                let mut entry = serde_json::Map::new();
                entry.insert("name".into(), Value::String(tool.name.clone()));
                entry.insert(
                    "description".into(),
                    Value::String(tool.description.clone()),
                );
                entry.insert("inputSchema".into(), tool.input_schema.clone());
                if tool.server_streaming {
                    entry.insert("_meta".into(), json!({"streaming": true}));
                }
                Value::Object(entry)
            })
            .collect()
    }

    pub async fn invoke(
        &self,
        tool_name: &str,
        request: Request<DynamicMessage>,
    ) -> Result<Response<DynamicMessage>, Status> {
        self.freeze();
        let tool = self
            .tool(tool_name)
            .ok_or_else(|| Status::not_found(format!("unknown tool {tool_name:?}")))?;
        let handler = tool.unary.clone().ok_or_else(|| {
            Status::failed_precondition(format!(
                "tool {tool_name:?} is server-streaming — use invoke_stream"
            ))
        })?;
        handler(request).await
    }

    pub async fn invoke_stream(
        &self,
        tool_name: &str,
        request: Request<DynamicMessage>,
    ) -> Result<Response<DynamicResponseStream>, Status> {
        self.freeze();
        let tool = self
            .tool(tool_name)
            .ok_or_else(|| Status::not_found(format!("unknown tool {tool_name:?}")))?;
        let handler = tool.stream.clone().ok_or_else(|| {
            Status::failed_precondition(format!("tool {tool_name:?} is unary — use invoke"))
        })?;
        let full_method = format!("/{}/{}", tool.service_full_name, tool.method_name);
        let response = handler(request).await?;
        Ok(response.map(|stream| recover_response_stream(stream, full_method)))
    }

    #[doc(hidden)]
    pub async fn invoke_typed_unary<Req, Resp, F, Fut>(
        &self,
        request: Request<Req>,
        info: ServerCallInfo,
        terminal: F,
    ) -> Result<Response<Resp>, Status>
    where
        Req: Send + 'static,
        Resp: Send + 'static,
        F: Fn(Request<Req>) -> Fut + Clone + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<Resp>, Status>> + Send + 'static,
    {
        self.run_typed(request, info, terminal, false).await
    }

    #[doc(hidden)]
    pub async fn invoke_typed_stream<Req, Item, F, Fut>(
        &self,
        request: Request<Req>,
        info: ServerCallInfo,
        terminal: F,
    ) -> Result<Response<BoxResponseStream<Item>>, Status>
    where
        Req: Send + 'static,
        Item: Send + 'static,
        F: Fn(Request<Req>) -> Fut + Clone + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<BoxResponseStream<Item>>, Status>>
            + Send
            + 'static,
    {
        let full_method = info.full_method.clone();
        let response = self.run_typed(request, info, terminal, true).await?;
        Ok(response.map(|stream| recover_response_stream(stream, full_method)))
    }

    #[doc(hidden)]
    pub async fn invoke_typed_stream_call<Req, Resp, F, Fut>(
        &self,
        request: Request<Req>,
        info: ServerCallInfo,
        terminal: F,
    ) -> Result<Response<Resp>, Status>
    where
        Req: Send + 'static,
        Resp: Send + 'static,
        F: Fn(Request<Req>) -> Fut + Clone + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<Resp>, Status>> + Send + 'static,
    {
        self.run_typed(request, info, terminal, true).await
    }

    async fn run_typed<Req, Resp, F, Fut>(
        &self,
        request: Request<Req>,
        info: ServerCallInfo,
        terminal: F,
        stream: bool,
    ) -> Result<Response<Resp>, Status>
    where
        Req: Send + 'static,
        Resp: Send + 'static,
        F: Fn(Request<Req>) -> Fut + Clone + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<Resp>, Status>> + Send + 'static,
    {
        let terminal: SharedHandler = Arc::new(move |request| {
            let terminal = terminal.clone();
            Box::pin(async move {
                let response = terminal(request.into_typed::<Req>()?).await?;
                Ok(ErasedResponse::new(response))
            })
        });
        let middleware = {
            let registry = self.inner.registry.read();
            if stream {
                registry.shared_stream_middleware.clone()
            } else {
                registry.shared_unary_middleware.clone()
            }
        };
        let has_shared_middleware = !middleware.is_empty();
        let mut current = terminal;
        for middleware in middleware.into_iter().rev() {
            let next = current.clone();
            let info = info.clone();
            current = Arc::new(move |request| middleware(request, info.clone(), next.clone()));
        }
        let method = info.full_method;
        let request = ErasedRequest::new(request);
        let invocation = if has_shared_middleware {
            std::panic::catch_unwind(AssertUnwindSafe(|| current(request))).map_err(|panic| {
                Status::internal(format!("panic in {method}: {}", panic_message(&panic)))
            })?
        } else {
            current(request)
        };
        match AssertUnwindSafe(invocation).catch_unwind().await {
            Ok(result) => result?.into_typed::<Resp>(),
            Err(panic) => Err(Status::internal(format!(
                "panic in {method}: {}",
                panic_message(&panic)
            ))),
        }
    }
}

fn ensure_configuring(registry: &Registry, operation: &str) -> Result<(), Status> {
    if registry.phase == Phase::Frozen {
        return Err(Status::failed_precondition(format!(
            "{operation} is not allowed after serving or invocation begins"
        )));
    }
    Ok(())
}

fn ensure_no_services(registry: &Registry, operation: &str) -> Result<(), Status> {
    if !registry.services.is_empty() {
        return Err(Status::failed_precondition(format!(
            "{operation} must be configured before service registration"
        )));
    }
    Ok(())
}

fn nonzero_or(value: usize, fallback: usize) -> usize {
    if value == 0 { fallback } else { value }
}

fn environment_patterns(name: &str) -> Vec<String> {
    std::env::var(name)
        .ok()
        .into_iter()
        .flat_map(|value| {
            value
                .split(',')
                .map(str::trim)
                .filter(|pattern| !pattern.is_empty())
                .map(str::to_string)
                .collect::<Vec<_>>()
        })
        .collect()
}

fn should_project(registry: &Registry, service: &str, method: &str) -> bool {
    let full_method = format!("{service}.{method}");
    if !registry.includes.is_empty()
        && !registry
            .includes
            .iter()
            .any(|pattern| glob_matches(pattern, &full_method))
    {
        return false;
    }
    !registry
        .excludes
        .iter()
        .any(|pattern| glob_matches(pattern, &full_method))
}

fn glob_matches(pattern: &str, candidate: &str) -> bool {
    let pattern = pattern.as_bytes();
    let candidate = candidate.as_bytes();
    let (mut pattern_index, mut candidate_index) = (0, 0);
    let (mut star, mut retry_candidate) = (None, 0);
    while candidate_index < candidate.len() {
        if pattern_index < pattern.len() && pattern[pattern_index] == candidate[candidate_index] {
            pattern_index += 1;
            candidate_index += 1;
        } else if pattern_index < pattern.len() && pattern[pattern_index] == b'*' {
            star = Some(pattern_index);
            pattern_index += 1;
            retry_candidate = candidate_index;
        } else if let Some(star_index) = star {
            pattern_index = star_index + 1;
            retry_candidate += 1;
            candidate_index = retry_candidate;
        } else {
            return false;
        }
    }
    while pattern_index < pattern.len() && pattern[pattern_index] == b'*' {
        pattern_index += 1;
    }
    pattern_index == pattern.len()
}

/// The default mapper forwards only W3C trace context, baggage, and a request
/// correlation ID. Authentication middleware should inject trusted identity by
/// another application-owned mechanism after validating credentials.
pub fn default_http_metadata_mapper(headers: &http::HeaderMap) -> MetadataMap {
    let mut metadata = MetadataMap::new();
    for key in ["traceparent", "tracestate", "baggage", "x-request-id"] {
        for value in headers.get_all(key) {
            if let Ok(value) = value.to_str()
                && let Ok(value) = value.parse()
            {
                metadata.append(key, value);
            }
        }
    }
    metadata
}

fn filter_incoming_metadata(mapped: MetadataMap) -> MetadataMap {
    let mut filtered = MetadataMap::new();
    for item in mapped.iter() {
        match item {
            KeyAndValueRef::Ascii(key, value) if !reserved_incoming_metadata(key.as_str()) => {
                filtered.append(key.clone(), value.clone());
            }
            KeyAndValueRef::Binary(key, value) if !reserved_incoming_metadata(key.as_str()) => {
                filtered.append_bin(key.clone(), value.clone());
            }
            KeyAndValueRef::Ascii(_, _) | KeyAndValueRef::Binary(_, _) => {}
        }
    }
    filtered
}

pub(crate) fn reserved_incoming_metadata(key: &str) -> bool {
    let key = key.to_ascii_lowercase();
    let logical_key = key.strip_suffix("-bin").unwrap_or(&key);
    if [
        "grpc-",
        "connect-",
        "invariant-internal-",
        "x-invariant-internal-",
        "x-tenant",
        "x-principal",
        "x-role",
        "x-user",
        "x-auth",
        "x-internal-",
        "internal-",
        "tenant-",
        "principal-",
        "role-",
        "user-",
        "auth-",
        "subject-",
        "identity-",
    ]
    .iter()
    .any(|prefix| logical_key.starts_with(prefix))
    {
        return true;
    }
    matches!(
        logical_key,
        "authorization"
            | "proxy-authorization"
            | "cookie"
            | "set-cookie"
            | "authentication"
            | "api-key"
            | "x-api-key"
            | "tenant"
            | "principal"
            | "role"
            | "user"
            | "subject"
            | "identity"
            | "te"
            | "host"
            | "connection"
            | "keep-alive"
            | "proxy-connection"
            | "transfer-encoding"
            | "upgrade"
            | "content-length"
            | "content-type"
            | "trailer"
    )
}

fn panic_message(panic: &Box<dyn Any + Send>) -> String {
    if let Some(message) = panic.downcast_ref::<&'static str>() {
        return (*message).to_string();
    }
    if let Some(message) = panic.downcast_ref::<String>() {
        return message.clone();
    }
    "<non-string panic>".to_string()
}

fn recover_response_stream<T>(
    mut stream: BoxResponseStream<T>,
    full_method: String,
) -> BoxResponseStream<T>
where
    T: Send + 'static,
{
    Box::pin(async_stream::stream! {
        loop {
            match AssertUnwindSafe(stream.next()).catch_unwind().await {
                Ok(Some(item)) => yield item,
                Ok(None) => break,
                Err(panic) => {
                    yield Err(Status::internal(format!(
                        "panic in {full_method}: {}",
                        panic_message(&panic)
                    )));
                    break;
                }
            }
        }
    })
}
