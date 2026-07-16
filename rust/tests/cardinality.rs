//! Generated tonic registration keeps client-streaming and bidi RPCs native.

#![allow(clippy::all)]

use futures::{StreamExt, TryStreamExt};
use invariant::{BoxResponseStream, Server, Status};
use std::sync::Arc;
use tonic::{Request, Response};

mod cardinality {
    include!(concat!(env!("OUT_DIR"), "/cardinality.v1.rs"));
}

#[derive(Clone)]
struct AllCardinality;

#[tonic::async_trait]
impl cardinality::all_cardinality_service_server::AllCardinalityService for AllCardinality {
    async fn unary(
        &self,
        request: Request<cardinality::Input>,
    ) -> Result<Response<cardinality::Output>, Status> {
        Ok(Response::new(cardinality::Output {
            value: request.into_inner().value,
        }))
    }

    type ServerStreamStream = BoxResponseStream<cardinality::Output>;

    async fn server_stream(
        &self,
        request: Request<cardinality::Input>,
    ) -> Result<Response<Self::ServerStreamStream>, Status> {
        let value = request.into_inner().value;
        let stream = futures::stream::iter([0, 1].map(move |index| {
            Ok(cardinality::Output {
                value: format!("{value}-{index}"),
            })
        }));
        Ok(Response::new(Box::pin(stream)))
    }

    async fn client_stream(
        &self,
        request: Request<tonic::Streaming<cardinality::Input>>,
    ) -> Result<Response<cardinality::Output>, Status> {
        let values = request
            .into_inner()
            .map(|item| item.map(|input| input.value))
            .try_collect::<Vec<_>>()
            .await?;
        Ok(Response::new(cardinality::Output {
            value: values.join(","),
        }))
    }

    type BidiStream = BoxResponseStream<cardinality::Output>;

    async fn bidi(
        &self,
        request: Request<tonic::Streaming<cardinality::Input>>,
    ) -> Result<Response<Self::BidiStream>, Status> {
        let stream = request.into_inner().map(|item| {
            item.map(|input| {
                if input.value == "panic" {
                    panic!("bidi-midstream-kaboom");
                }
                cardinality::Output {
                    value: format!("echo:{}", input.value),
                }
            })
        });
        Ok(Response::new(Box::pin(stream)))
    }
}

#[tokio::test]
async fn full_generated_service_remains_native_while_projections_stay_bounded() {
    let server = Arc::new(
        Server::from_bytes(include_bytes!(concat!(
            env!("OUT_DIR"),
            "/cardinality.binpb"
        )))
        .unwrap(),
    );
    cardinality::register_all_cardinality_service_server(&server, AllCardinality).unwrap();
    assert_eq!(
        server
            .tool_catalog()
            .into_iter()
            .map(|tool| tool["name"].as_str().unwrap().to_string())
            .collect::<Vec<_>>(),
        [
            "AllCardinalityService.ServerStream",
            "AllCardinalityService.Unary"
        ]
    );

    let routes = server.grpc_routes();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_routes(routes)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await
            .unwrap();
    });
    let mut client =
        cardinality::all_cardinality_service_client::AllCardinalityServiceClient::connect(format!(
            "http://{address}"
        ))
        .await
        .unwrap();

    assert_eq!(
        client
            .unary(cardinality::Input { value: "u".into() })
            .await
            .unwrap()
            .into_inner()
            .value,
        "u"
    );
    let mut server_stream = client
        .server_stream(cardinality::Input { value: "s".into() })
        .await
        .unwrap()
        .into_inner();
    assert_eq!(server_stream.message().await.unwrap().unwrap().value, "s-0");
    assert_eq!(server_stream.message().await.unwrap().unwrap().value, "s-1");

    let client_stream = futures::stream::iter(["a", "b"].map(|value| cardinality::Input {
        value: value.into(),
    }));
    assert_eq!(
        client
            .client_stream(client_stream)
            .await
            .unwrap()
            .into_inner()
            .value,
        "a,b"
    );

    let bidi = futures::stream::iter(["x", "y"].map(|value| cardinality::Input {
        value: value.into(),
    }));
    let mut bidi = client.bidi(bidi).await.unwrap().into_inner();
    assert_eq!(bidi.message().await.unwrap().unwrap().value, "echo:x");
    assert_eq!(bidi.message().await.unwrap().unwrap().value, "echo:y");

    let bidi = futures::stream::iter(["first", "panic"].map(|value| cardinality::Input {
        value: value.into(),
    }));
    let mut bidi = client.bidi(bidi).await.unwrap().into_inner();
    assert_eq!(bidi.message().await.unwrap().unwrap().value, "echo:first");
    let status = bidi.message().await.unwrap_err();
    assert_eq!(status.code(), tonic::Code::Internal);
    assert!(status.message().contains("bidi-midstream-kaboom"));
    assert!(
        status
            .message()
            .contains("/cardinality.v1.AllCardinalityService/Bidi")
    );
    task.abort();
}
