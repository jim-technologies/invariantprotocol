//! Generated service fixtures shared by the Rust integration tests.

#![allow(clippy::all)]
#![allow(dead_code)]

use futures::future::BoxFuture;
use invariant::{BoxResponseStream, Server, Status};
use prost::Message;
use prost_types::FileDescriptorProto;
use std::collections::BTreeSet;
use std::sync::Arc;
use tonic::{Request, Response};

pub mod greet {
    include!(concat!(env!("OUT_DIR"), "/greet.v1.rs"));
}

pub const DESCRIPTOR_PATH: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../python/tests/proto/descriptor.binpb"
);

type GreetFn = Arc<
    dyn Fn(
            Request<greet::GreetRequest>,
        ) -> BoxFuture<'static, Result<Response<greet::GreetResponse>, Status>>
        + Send
        + Sync,
>;
type GroupFn = Arc<
    dyn Fn(
            Request<greet::GreetGroupRequest>,
        ) -> BoxFuture<'static, Result<Response<greet::GreetGroupResponse>, Status>>
        + Send
        + Sync,
>;
type StreamFn = Arc<
    dyn Fn(
            Request<greet::StreamGreetRequest>,
        )
            -> BoxFuture<'static, Result<Response<BoxResponseStream<greet::GreetResponse>>, Status>>
        + Send
        + Sync,
>;

#[derive(Clone)]
pub struct TestGreetService {
    greet: GreetFn,
    group: GroupFn,
    stream: StreamFn,
}

impl Default for TestGreetService {
    fn default() -> Self {
        Self {
            greet: Arc::new(|request| {
                Box::pin(async move {
                    let request = request.into_inner();
                    Ok(Response::new(greet::GreetResponse {
                        message: format!("Hi {}", request.name),
                        mood: request.mood.unwrap_or_default(),
                        tags: request.tags,
                    }))
                })
            }),
            group: Arc::new(|request| {
                Box::pin(async move {
                    let messages = request
                        .into_inner()
                        .people
                        .into_iter()
                        .map(|person| format!("Hi {}", person.name))
                        .collect::<Vec<_>>();
                    Ok(Response::new(greet::GreetGroupResponse {
                        count: messages.len() as i32,
                        messages,
                    }))
                })
            }),
            stream: Arc::new(|request| {
                Box::pin(async move {
                    let request = request.into_inner();
                    let count = request.count.max(1);
                    let name = request.name;
                    let stream = futures::stream::iter((0..count).map(move |index| {
                        Ok(greet::GreetResponse {
                            message: format!("Hi {name} #{index}"),
                            ..Default::default()
                        })
                    }));
                    Ok(Response::new(Box::pin(stream) as BoxResponseStream<_>))
                })
            }),
        }
    }
}

impl TestGreetService {
    pub fn with_greet<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(Request<greet::GreetRequest>) -> Fut + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<greet::GreetResponse>, Status>>
            + Send
            + 'static,
    {
        self.greet = Arc::new(move |request| Box::pin(handler(request)));
        self
    }

    pub fn with_group<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(Request<greet::GreetGroupRequest>) -> Fut + Send + Sync + 'static,
        Fut: std::future::Future<Output = Result<Response<greet::GreetGroupResponse>, Status>>
            + Send
            + 'static,
    {
        self.group = Arc::new(move |request| Box::pin(handler(request)));
        self
    }

    pub fn with_stream<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(Request<greet::StreamGreetRequest>) -> Fut + Send + Sync + 'static,
        Fut: std::future::Future<
                Output = Result<Response<BoxResponseStream<greet::GreetResponse>>, Status>,
            > + Send
            + 'static,
    {
        self.stream = Arc::new(move |request| Box::pin(handler(request)));
        self
    }
}

#[tonic::async_trait]
impl greet::greet_service_server::GreetService for TestGreetService {
    async fn greet(
        &self,
        request: Request<greet::GreetRequest>,
    ) -> Result<Response<greet::GreetResponse>, Status> {
        (self.greet)(request).await
    }

    async fn greet_group(
        &self,
        request: Request<greet::GreetGroupRequest>,
    ) -> Result<Response<greet::GreetGroupResponse>, Status> {
        (self.group)(request).await
    }

    type StreamGreetStream = BoxResponseStream<greet::GreetResponse>;

    async fn stream_greet(
        &self,
        request: Request<greet::StreamGreetRequest>,
    ) -> Result<Response<Self::StreamGreetStream>, Status> {
        (self.stream)(request).await
    }
}

pub fn registered_server(service: TestGreetService) -> Arc<Server> {
    let server = Arc::new(Server::from_descriptor(DESCRIPTOR_PATH).expect("load descriptor"));
    greet::register_greet_service_server(&server, service).expect("generated service registration");
    server
}

pub async fn serve_native(
    server: Arc<Server>,
) -> (std::net::SocketAddr, tokio::task::JoinHandle<()>) {
    let routes = server.grpc_routes();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind native gRPC");
    let address = listener.local_addr().expect("listener address");
    let task = tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_routes(routes)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await
            .expect("serve native gRPC");
    });
    (address, task)
}

pub async fn generated_client(
    address: std::net::SocketAddr,
) -> greet::greet_service_client::GreetServiceClient<tonic::transport::Channel> {
    greet::greet_service_client::GreetServiceClient::connect(format!("http://{address}"))
        .await
        .expect("connect generated client")
}

pub async fn reflected_service_names(address: std::net::SocketAddr) -> BTreeSet<String> {
    use tonic_reflection::pb::v1::server_reflection_request::MessageRequest;
    use tonic_reflection::pb::v1::server_reflection_response::MessageResponse;

    match reflection_response(address, MessageRequest::ListServices(String::new()))
        .await
        .expect("reflection service list")
    {
        MessageResponse::ListServicesResponse(response) => response
            .service
            .into_iter()
            .map(|service| service.name)
            .collect(),
        response => panic!("expected reflection service list, got {response:?}"),
    }
}

pub async fn reflection_has_file_for_symbol(address: std::net::SocketAddr, symbol: &str) -> bool {
    use tonic_reflection::pb::v1::server_reflection_request::MessageRequest;
    use tonic_reflection::pb::v1::server_reflection_response::MessageResponse;

    matches!(
        reflection_response(
            address,
            MessageRequest::FileContainingSymbol(symbol.to_string()),
        )
        .await,
        Ok(MessageResponse::FileDescriptorResponse(_))
    )
}

pub async fn reflected_method_names(
    address: std::net::SocketAddr,
    service_name: &str,
) -> BTreeSet<String> {
    use tonic_reflection::pb::v1::server_reflection_request::MessageRequest;
    use tonic_reflection::pb::v1::server_reflection_response::MessageResponse;

    let response = reflection_response(
        address,
        MessageRequest::FileContainingSymbol(service_name.to_string()),
    )
    .await
    .expect("reflection file for service");
    let MessageResponse::FileDescriptorResponse(response) = response else {
        panic!("expected reflection file response");
    };
    for bytes in response.file_descriptor_proto {
        let file = FileDescriptorProto::decode(bytes.as_slice()).expect("decode reflected file");
        let package = file.package.as_deref().unwrap_or_default();
        for service in file.service {
            let full_name = if package.is_empty() {
                service.name().to_string()
            } else {
                format!("{package}.{}", service.name())
            };
            if full_name == service_name {
                return service
                    .method
                    .into_iter()
                    .map(|method| method.name().to_string())
                    .collect();
            }
        }
    }
    panic!("reflected descriptor omitted service {service_name}");
}

async fn reflection_response(
    address: std::net::SocketAddr,
    request: tonic_reflection::pb::v1::server_reflection_request::MessageRequest,
) -> Result<tonic_reflection::pb::v1::server_reflection_response::MessageResponse, tonic::Status> {
    let channel = tonic::transport::Channel::from_shared(format!("http://{address}"))
        .unwrap()
        .connect()
        .await
        .unwrap();
    let mut client =
        tonic_reflection::pb::v1::server_reflection_client::ServerReflectionClient::new(channel);
    let response = client
        .server_reflection_info(futures::stream::iter([
            tonic_reflection::pb::v1::ServerReflectionRequest {
                host: String::new(),
                message_request: Some(request),
            },
        ]))
        .await
        .unwrap()
        .into_inner()
        .message()
        .await?
        .expect("reflection response");
    Ok(response.message_response.expect("reflection response kind"))
}
