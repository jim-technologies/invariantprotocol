//! Native gRPC support for generated services registered with [`Server`].
//!
//! There is deliberately no descriptor-driven gRPC frame parser here. Tonic's
//! generated service, codec, metadata, status, deadline, cancellation, and
//! streaming machinery are the canonical native transport.

use crate::server::{Server, UnaryProjectionHandler};
use prost::Message;
use prost_reflect::{DynamicMessage, MessageDescriptor};
use prost_types::FileDescriptorSet;
use std::sync::Arc;
use tonic::body::Body;
use tonic::client::{Grpc, GrpcService};
use tonic::codec::{Codec, DecodeBuf, Decoder, EncodeBuf, Encoder};
use tonic::codegen::{Body as HttpBody, StdError};
use tonic::{Request, Status};

pub(crate) fn build_reflection(
    server: &Server,
) -> Result<
    tonic_reflection::server::v1::ServerReflectionServer<
        impl tonic_reflection::server::v1::ServerReflection,
    >,
    tonic_reflection::server::Error,
> {
    let reflected_services = server.reflected_service_names();
    let mut descriptors = FileDescriptorSet::decode(server.parsed().raw_fds.as_slice())
        .expect("descriptor image was validated when the server was constructed");
    for file in &mut descriptors.file {
        let package = file.package.as_deref().unwrap_or_default();
        file.service.retain(|service| {
            let name = if package.is_empty() {
                service.name().to_string()
            } else {
                format!("{package}.{}", service.name())
            };
            reflected_services.contains(&name)
        });
    }

    let mut builder = tonic_reflection::server::Builder::configure()
        .register_file_descriptor_set(descriptors)
        .with_service_name("grpc.reflection.v1.ServerReflection");
    for service in reflected_services {
        builder = builder.with_service_name(service);
    }
    builder.build_v1()
}

#[derive(Clone)]
pub(crate) struct DynamicCodec {
    decode: MessageDescriptor,
}

impl DynamicCodec {
    pub(crate) fn new(decode: MessageDescriptor) -> Self {
        Self { decode }
    }
}

pub(crate) struct DynamicEncoder;

impl Encoder for DynamicEncoder {
    type Item = DynamicMessage;
    type Error = Status;

    fn encode(&mut self, item: Self::Item, destination: &mut EncodeBuf<'_>) -> Result<(), Status> {
        item.encode(destination)
            .map_err(|error| Status::internal(format!("encode protobuf message: {error}")))
    }
}

pub(crate) struct DynamicDecoder {
    descriptor: MessageDescriptor,
}

impl Decoder for DynamicDecoder {
    type Item = DynamicMessage;
    type Error = Status;

    fn decode(&mut self, source: &mut DecodeBuf<'_>) -> Result<Option<Self::Item>, Status> {
        DynamicMessage::decode(self.descriptor.clone(), source)
            .map(Some)
            .map_err(|error| Status::internal(format!("decode protobuf message: {error}")))
    }
}

impl Codec for DynamicCodec {
    type Encode = DynamicMessage;
    type Decode = DynamicMessage;
    type Encoder = DynamicEncoder;
    type Decoder = DynamicDecoder;

    fn encoder(&mut self) -> Self::Encoder {
        DynamicEncoder
    }

    fn decoder(&mut self) -> Self::Decoder {
        DynamicDecoder {
            descriptor: self.decode.clone(),
        }
    }
}

pub(crate) fn remote_unary_handler<T, F>(
    service: T,
    configure: F,
    full_method: http::uri::PathAndQuery,
    output: MessageDescriptor,
) -> UnaryProjectionHandler
where
    T: GrpcService<Body> + Clone + Send + Sync + 'static,
    T::ResponseBody: HttpBody<Data = tonic::codegen::Bytes> + Send + 'static,
    <T::ResponseBody as HttpBody>::Error: Into<StdError> + Send,
    T::Error: Into<StdError>,
    T::Future: Send + 'static,
    F: Fn(Grpc<T>) -> Grpc<T> + Clone + Send + Sync + 'static,
{
    Arc::new(move |request: Request<DynamicMessage>| {
        let mut client = configure.clone()(Grpc::new(service.clone()));
        let full_method = full_method.clone();
        let codec = DynamicCodec::new(output.clone());
        Box::pin(async move {
            client.ready().await.map_err(|error| {
                Status::unavailable(format!(
                    "remote gRPC service is not ready: {}",
                    error.into()
                ))
            })?;
            client.unary(request, full_method, codec).await
        })
    })
}

pub(crate) async fn serve_remote_unary(
    handler: UnaryProjectionHandler,
    input: MessageDescriptor,
    request: http::Request<axum::body::Body>,
) -> http::Response<Body> {
    let service = tower::service_fn(move |request| {
        let handler = handler.clone();
        async move { handler(request).await }
    });
    tonic::server::Grpc::new(DynamicCodec::new(input))
        .unary(service, request)
        .await
}
