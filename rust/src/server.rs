//! Core `Server` type — descriptor + tool registry + interceptor chain +
//! in-process `invoke` / `invoke_stream` dispatch. All projections (HTTP,
//! gRPC, MCP, CLI) sit on top of this without re-implementing the chain.
//!
//! ## Why per-method `register_unary` instead of `register(servicer)`
//!
//! Go and Python use runtime reflection to match servicer methods to RPC
//! names. Rust has no equivalent (no method-name reflection on `Trait`s).
//! The honest options are:
//!
//! 1. Per-method explicit registration. Chosen here. ~1 line per RPC,
//!    no codegen, no macros, no procedural plumbing.
//! 2. Generated trait + `register(impl MyTrait)`. Requires a per-service
//!    proc-macro that emits both the trait and the dispatch table. More
//!    magic, more code to maintain, no behavioural win.
//!
//! Option 1 wins for "thin and opinionated".

use crate::descriptor::ParsedDescriptor;
use crate::errors::{Code, Status};
use crate::schema::SchemaGen;
use crate::stream::{dyn_stream_channel, DynStreamTx, ServerStreamTx, STREAM_BUFFER};
use futures::future::BoxFuture;
use futures::stream::BoxStream;
use futures::FutureExt;
use parking_lot::RwLock;
use prost::Message;
use prost_reflect::{DynamicMessage, MessageDescriptor};
use serde_json::{json, Value};
use std::collections::BTreeMap;
use std::panic::AssertUnwindSafe;
use std::sync::Arc;

pub const SERVER_NAME: &str = "invariant-protocol";
pub const SERVER_VERSION: &str = env!("CARGO_PKG_VERSION");

/// Metadata about the RPC being invoked. Passed to every interceptor; mirrors
/// Go's `ServerCallInfo` and Python's `ServerCallInfo`.
#[derive(Debug, Clone)]
pub struct ServerCallInfo {
    pub full_method: String,
}

/// Type-erased unary handler. The boundary already speaks `DynamicMessage` so
/// every projection can call any tool without knowing static types.
pub type UnaryHandler =
    Arc<dyn Fn(DynamicMessage) -> BoxFuture<'static, Result<DynamicMessage, Status>> + Send + Sync>;

/// Unary interceptor signature, shape-mirroring `grpc.UnaryServerInterceptor`.
pub type UnaryInterceptor = Arc<
    dyn Fn(
            DynamicMessage,
            ServerCallInfo,
            UnaryHandler,
        ) -> BoxFuture<'static, Result<DynamicMessage, Status>>
        + Send
        + Sync,
>;

/// Type-erased server-streaming handler — drives the supplied DynStreamTx.
pub type StreamHandler = Arc<
    dyn Fn(DynamicMessage, Arc<DynStreamTx>) -> BoxFuture<'static, Result<(), Status>>
        + Send
        + Sync,
>;

/// Stream interceptor signature. Wraps the entire call — mirrors
/// `grpc.StreamServerInterceptor`. Per-message hooks: wrap the user's handler
/// directly, not the interceptor.
pub type StreamInterceptor = Arc<
    dyn Fn(
            DynamicMessage,
            Arc<DynStreamTx>,
            ServerCallInfo,
            StreamHandler,
        ) -> BoxFuture<'static, Result<(), Status>>
        + Send
        + Sync,
>;

/// One registered RPC method projected as a tool.
pub struct Tool {
    pub name: String,
    pub description: String,
    pub input_schema: Value,
    pub input_type: String,
    pub output_type: String,
    pub service_full_name: String,
    pub method_name: String,
    pub server_streaming: bool,

    pub(crate) handler: Option<UnaryHandler>,
    pub(crate) stream_handler: Option<StreamHandler>,
    pub(crate) call_info: ServerCallInfo,
    pub(crate) input_desc: MessageDescriptor,
    pub(crate) output_desc: MessageDescriptor,
}

pub struct Server {
    parsed: ParsedDescriptor,
    tools: RwLock<BTreeMap<String, Arc<Tool>>>,
    interceptors: RwLock<Vec<UnaryInterceptor>>,
    stream_interceptors: RwLock<Vec<StreamInterceptor>>,
    // Body-size safety caps for HTTP / Connect projections. Defaults are
    // tight; raise per-server via the setters when the application has a
    // legitimate need. Mirrors Go's `httpMaxUnaryRequest` /
    // `connectStreamMaxRequest` fields.
    http_max_unary_request: RwLock<usize>,
    connect_stream_max_request: RwLock<usize>,
}

impl Server {
    pub fn from_descriptor(path: &str) -> Result<Self, Status> {
        let parsed = ParsedDescriptor::from_file(path)?;
        Ok(Self::with_parsed(parsed))
    }

    pub fn from_bytes(data: &[u8]) -> Result<Self, Status> {
        let parsed = ParsedDescriptor::from_bytes(data)?;
        Ok(Self::with_parsed(parsed))
    }

    fn with_parsed(parsed: ParsedDescriptor) -> Self {
        Self {
            parsed,
            tools: RwLock::new(BTreeMap::new()),
            interceptors: RwLock::new(Vec::new()),
            stream_interceptors: RwLock::new(Vec::new()),
            http_max_unary_request: RwLock::new(crate::projections::http::HTTP_MAX_UNARY_REQUEST),
            connect_stream_max_request: RwLock::new(
                crate::projections::http::CONNECT_STREAM_MAX_REQUEST,
            ),
        }
    }

    /// Override the HTTP unary body-size cap. Pass 0 to reset to the 16 MiB
    /// default. Mirrors Go's `SetMaxUnaryRequestBytes` and Python's
    /// `set_max_unary_request_bytes`.
    pub fn set_max_unary_request_bytes(&self, n: usize) {
        let value = if n == 0 {
            crate::projections::http::HTTP_MAX_UNARY_REQUEST
        } else {
            n
        };
        *self.http_max_unary_request.write() = value;
    }

    /// Override the Connect streaming request envelope cap. Pass 0 to reset
    /// to the 16 MiB default.
    pub fn set_max_stream_request_bytes(&self, n: usize) {
        let value = if n == 0 {
            crate::projections::http::CONNECT_STREAM_MAX_REQUEST
        } else {
            n
        };
        *self.connect_stream_max_request.write() = value;
    }

    /// Current HTTP unary body-size cap.
    pub fn max_unary_request_bytes(&self) -> usize {
        *self.http_max_unary_request.read()
    }

    /// Current Connect streaming request envelope cap.
    pub fn max_stream_request_bytes(&self) -> usize {
        *self.connect_stream_max_request.read()
    }

    /// Register a unary handler against a tool name (`"GreetService.Greet"`).
    ///
    /// `Req` and `Resp` must be `prost::Message` types whose wire format matches
    /// the proto descriptor's input/output for this method. The framework
    /// performs a binary roundtrip between `DynamicMessage` (the projection
    /// boundary type) and your typed `Req` / `Resp`; same trick Go uses with
    /// `dynamicpb`. Re-registering an existing tool name overwrites silently —
    /// matching Go's behaviour when re-running `Register`.
    pub fn register_unary<Req, Resp, F, Fut>(&self, tool_name: &str, f: F) -> &Self
    where
        Req: Message + Default + Send + 'static,
        Resp: Message + Send + 'static,
        F: Fn(Req) -> Fut + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Resp, Status>> + Send + 'static,
    {
        let (svc_full, method_name) = match split_tool_name(&self.parsed, tool_name) {
            Some(parts) => parts,
            None => {
                // Defer the error to invoke time — same pattern as Go's addTool.
                let err = Status::not_found(format!("unknown tool name: {tool_name}"));
                let handler: UnaryHandler = Arc::new(move |_| {
                    let err = err.clone();
                    Box::pin(async move { Err(err) })
                });
                self.tools.write().insert(
                    tool_name.to_string(),
                    Arc::new(Tool {
                        name: tool_name.to_string(),
                        description: tool_name.to_string(),
                        input_schema: json!({"type": "object"}),
                        input_type: String::new(),
                        output_type: String::new(),
                        service_full_name: String::new(),
                        method_name: String::new(),
                        server_streaming: false,
                        handler: Some(handler),
                        stream_handler: None,
                        call_info: ServerCallInfo {
                            full_method: String::new(),
                        },
                        input_desc: self.parsed.pool.all_messages().next().unwrap(),
                        output_desc: self.parsed.pool.all_messages().next().unwrap(),
                    }),
                );
                return self;
            }
        };

        let svc = self
            .parsed
            .services
            .get(&svc_full)
            .expect("service exists; checked above");
        let method = svc
            .methods
            .get(&method_name)
            .expect("method exists; checked above");
        let input_desc = self
            .parsed
            .pool
            .get_message_by_name(&method.input_type)
            .expect("input descriptor present");
        let output_desc = self
            .parsed
            .pool
            .get_message_by_name(&method.output_type)
            .expect("output descriptor present");

        let output_desc_for_handler = output_desc.clone();
        let handler: UnaryHandler = Arc::new(move |dyn_req: DynamicMessage| {
            let bytes = dyn_req.encode_to_vec();
            let typed = match Req::decode(&bytes[..]) {
                Ok(v) => v,
                Err(e) => {
                    return Box::pin(async move {
                        Err(Status::invalid_argument(format!("decode request: {e}")))
                    });
                }
            };
            let fut = f(typed);
            let output_desc = output_desc_for_handler.clone();
            Box::pin(async move {
                let resp = fut.await?;
                let buf = resp.encode_to_vec();
                DynamicMessage::decode(output_desc, &buf[..])
                    .map_err(|e| Status::internal(format!("encode response: {e}")))
            })
        });

        let schema = SchemaGen::new(&self.parsed).message_to_schema(&method.input_type);
        let description = if method.comment.is_empty() {
            tool_name.to_string()
        } else {
            method.comment.clone()
        };

        let tool = Tool {
            name: tool_name.to_string(),
            description,
            input_schema: schema,
            input_type: method.input_type.clone(),
            output_type: method.output_type.clone(),
            service_full_name: svc_full.clone(),
            method_name: method_name.clone(),
            server_streaming: method.server_streaming,
            handler: Some(handler),
            stream_handler: None,
            call_info: ServerCallInfo {
                full_method: format!("/{svc_full}/{method_name}"),
            },
            input_desc,
            output_desc,
        };

        self.tools
            .write()
            .insert(tool_name.to_string(), Arc::new(tool));
        self
    }

    /// Register a server-streaming handler. The handler signature is
    /// `async fn(Req, ServerStreamTx<Resp>) -> Result<(), Status>` — call
    /// `tx.send(resp).await` per emitted message. Mirrors Go's `ServerStream`
    /// interface and Python's async-generator handlers.
    pub fn register_stream<Req, Resp, F, Fut>(&self, tool_name: &str, f: F) -> &Self
    where
        Req: Message + Default + Send + 'static,
        Resp: Message + Send + 'static,
        F: Fn(Req, ServerStreamTx<Resp>) -> Fut + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<(), Status>> + Send + 'static,
    {
        let Some((svc_full, method_name)) = split_tool_name(&self.parsed, tool_name) else {
            // Tool name unknown — defer the error to invoke time, same as register_unary.
            let err = Status::not_found(format!("unknown tool name: {tool_name}"));
            let handler: StreamHandler = Arc::new(move |_req, _tx| {
                let err = err.clone();
                Box::pin(async move { Err(err) })
            });
            let any_desc = self.parsed.pool.all_messages().next().unwrap();
            self.tools.write().insert(
                tool_name.to_string(),
                Arc::new(Tool {
                    name: tool_name.to_string(),
                    description: tool_name.to_string(),
                    input_schema: json!({"type": "object"}),
                    input_type: String::new(),
                    output_type: String::new(),
                    service_full_name: String::new(),
                    method_name: String::new(),
                    server_streaming: true,
                    handler: None,
                    stream_handler: Some(handler),
                    call_info: ServerCallInfo {
                        full_method: String::new(),
                    },
                    input_desc: any_desc.clone(),
                    output_desc: any_desc,
                }),
            );
            return self;
        };

        let svc = self.parsed.services.get(&svc_full).expect("service exists");
        let method = svc.methods.get(&method_name).expect("method exists");
        let input_desc = self
            .parsed
            .pool
            .get_message_by_name(&method.input_type)
            .expect("input descriptor present");
        let output_desc = self
            .parsed
            .pool
            .get_message_by_name(&method.output_type)
            .expect("output descriptor present");

        let handler: StreamHandler = Arc::new(move |dyn_req: DynamicMessage, tx_inner| {
            let bytes = dyn_req.encode_to_vec();
            let typed = match Req::decode(&bytes[..]) {
                Ok(v) => v,
                Err(e) => {
                    return Box::pin(async move {
                        Err(Status::invalid_argument(format!("decode request: {e}")))
                    });
                }
            };
            let tx = ServerStreamTx::<Resp>::new(tx_inner);
            Box::pin(f(typed, tx))
        });

        let schema = SchemaGen::new(&self.parsed).message_to_schema(&method.input_type);
        let description = if method.comment.is_empty() {
            tool_name.to_string()
        } else {
            method.comment.clone()
        };

        let tool = Tool {
            name: tool_name.to_string(),
            description,
            input_schema: schema,
            input_type: method.input_type.clone(),
            output_type: method.output_type.clone(),
            service_full_name: svc_full.clone(),
            method_name: method_name.clone(),
            server_streaming: true,
            handler: None,
            stream_handler: Some(handler),
            call_info: ServerCallInfo {
                full_method: format!("/{svc_full}/{method_name}"),
            },
            input_desc,
            output_desc,
        };
        self.tools
            .write()
            .insert(tool_name.to_string(), Arc::new(tool));
        self
    }

    /// Register a unary interceptor. First registered = outermost.
    pub fn use_interceptor(&self, interceptor: UnaryInterceptor) -> &Self {
        self.interceptors.write().push(interceptor);
        self
    }

    /// Register a stream interceptor. First registered = outermost. Stream
    /// interceptors wrap the whole call — for per-message hooks, wrap the
    /// handler directly.
    pub fn use_stream_interceptor(&self, interceptor: StreamInterceptor) -> &Self {
        self.stream_interceptors.write().push(interceptor);
        self
    }

    /// Look up a tool by name. Returns `None` if not registered.
    pub fn tool(&self, name: &str) -> Option<Arc<Tool>> {
        self.tools.read().get(name).cloned()
    }

    /// Snapshot of registered tools.
    pub fn tools_snapshot(&self) -> Vec<Arc<Tool>> {
        self.tools.read().values().cloned().collect()
    }

    pub fn parsed(&self) -> &ParsedDescriptor {
        &self.parsed
    }

    /// Tool catalog, identical wire shape to Go's `ToolCatalog()` and
    /// Python's `tool_catalog()` — same key order, same `_meta.streaming`
    /// annotation on streaming tools.
    pub fn tool_catalog(&self) -> Vec<Value> {
        let tools = self.tools.read();
        let mut names: Vec<&String> = tools.keys().collect();
        names.sort();
        names
            .into_iter()
            .map(|name| {
                let t = tools.get(name).unwrap();
                let mut entry = serde_json::Map::new();
                entry.insert("name".into(), Value::String(t.name.clone()));
                entry.insert("description".into(), Value::String(t.description.clone()));
                entry.insert("inputSchema".into(), t.input_schema.clone());
                if t.server_streaming {
                    entry.insert("_meta".into(), json!({"streaming": true}));
                }
                Value::Object(entry)
            })
            .collect()
    }

    /// In-process unary dispatch — mirrors Go's `Server.Invoke` and Python's
    /// `Server.invoke`. The caller passes a `DynamicMessage` so this can be
    /// driven from any boundary; helper crates may wrap with typed inputs.
    pub async fn invoke(
        &self,
        tool_name: &str,
        request: DynamicMessage,
    ) -> Result<DynamicMessage, Status> {
        let tool = self
            .tool(tool_name)
            .ok_or_else(|| Status::not_found(format!("unknown tool {tool_name:?}")))?;
        if tool.server_streaming {
            return Err(Status::failed_precondition(format!(
                "tool {tool_name:?} is server-streaming — use invoke_stream"
            )));
        }
        self.chained_invoke(tool, request).await
    }

    /// In-process server-streaming dispatch. Returns a stream of typed
    /// `DynamicMessage` responses — projections own the receiver side.
    /// Mirrors Go's `InvokeStream` and Python's `invoke_stream`.
    pub fn invoke_stream(
        self: &Arc<Self>,
        tool_name: &str,
        request: DynamicMessage,
    ) -> BoxStream<'static, Result<DynamicMessage, Status>> {
        let server = self.clone();
        let tool_name = tool_name.to_string();
        let pool = self.parsed.pool.clone();
        Box::pin(async_stream::stream! {
            let Some(tool) = server.tool(&tool_name) else {
                yield Err(Status::not_found(format!("unknown tool {tool_name:?}")));
                return;
            };
            if !tool.server_streaming {
                yield Err(Status::failed_precondition(format!(
                    "tool {tool_name:?} is unary — use invoke"
                )));
                return;
            }
            let output_desc = tool.output_desc.clone();
            let _ = pool; // keep pool alive; not strictly required but documents intent
            let (tx, mut rx) = dyn_stream_channel(output_desc, STREAM_BUFFER);
            let server_for_handler = server.clone();
            let tool_for_handler = tool.clone();
            let req_for_handler = request;
            let handle = tokio::spawn(async move {
                server_for_handler.chained_invoke_stream(tool_for_handler, req_for_handler, tx).await
            });

            use futures::StreamExt;
            while let Some(msg) = rx.next().await {
                yield Ok(msg);
            }
            match handle.await {
                Ok(Ok(())) => {}
                Ok(Err(e)) => yield Err(e),
                Err(join_err) => yield Err(Status::internal(format!("stream join: {join_err}"))),
            }
        })
    }

    /// Run the unary interceptor chain then the handler. First registered = outermost.
    /// Panics in any layer become `codes.Internal` status errors — a single
    /// goroutine-style bug must not be allowed to crash the whole server.
    pub(crate) async fn chained_invoke(
        &self,
        tool: Arc<Tool>,
        request: DynamicMessage,
    ) -> Result<DynamicMessage, Status> {
        let Some(handler) = tool.handler.clone() else {
            return Err(Status::internal("unary handler not registered"));
        };
        let interceptors = self.interceptors.read().clone();
        let mut current: UnaryHandler = handler;
        for interceptor in interceptors.into_iter().rev() {
            let next = current.clone();
            let info = tool.call_info.clone();
            current = Arc::new(move |req| {
                let interceptor = interceptor.clone();
                let next = next.clone();
                let info = info.clone();
                interceptor(req, info, next)
            });
        }
        let method = tool.call_info.full_method.clone();
        match AssertUnwindSafe(current(request)).catch_unwind().await {
            Ok(result) => result,
            Err(panic) => Err(Status::new(
                Code::Internal,
                format!("panic in {method}: {}", panic_message(&panic)),
            )),
        }
    }

    /// Run the stream interceptor chain then the streaming handler. Panics
    /// in any layer become `codes.Internal` status errors, same as unary.
    pub(crate) async fn chained_invoke_stream(
        &self,
        tool: Arc<Tool>,
        request: DynamicMessage,
        tx: Arc<DynStreamTx>,
    ) -> Result<(), Status> {
        let Some(handler) = tool.stream_handler.clone() else {
            return Err(Status::internal("stream handler not registered"));
        };
        let interceptors = self.stream_interceptors.read().clone();
        let mut current: StreamHandler = handler;
        for interceptor in interceptors.into_iter().rev() {
            let next = current.clone();
            let info = tool.call_info.clone();
            current = Arc::new(move |req, tx| {
                let interceptor = interceptor.clone();
                let next = next.clone();
                let info = info.clone();
                interceptor(req, tx, info, next)
            });
        }
        let method = tool.call_info.full_method.clone();
        match AssertUnwindSafe(current(request, tx)).catch_unwind().await {
            Ok(result) => result,
            Err(panic) => Err(Status::new(
                Code::Internal,
                format!("panic in {method}: {}", panic_message(&panic)),
            )),
        }
    }
}

fn panic_message(p: &Box<dyn std::any::Any + Send>) -> String {
    if let Some(s) = p.downcast_ref::<&'static str>() {
        return (*s).to_string();
    }
    if let Some(s) = p.downcast_ref::<String>() {
        return s.clone();
    }
    "<non-string panic>".to_string()
}

fn split_tool_name(parsed: &ParsedDescriptor, tool_name: &str) -> Option<(String, String)> {
    // Tool name shape: `ServiceName.MethodName`. We resolve the simple
    // service name back to its package-qualified name via the descriptor.
    let dot = tool_name.rfind('.')?;
    let simple_svc = &tool_name[..dot];
    let method = &tool_name[dot + 1..];
    for (full_name, svc) in &parsed.services {
        if svc.name == simple_svc && svc.methods.contains_key(method) {
            return Some((full_name.clone(), method.to_string()));
        }
    }
    None
}
