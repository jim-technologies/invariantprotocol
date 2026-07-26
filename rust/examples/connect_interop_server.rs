//! Serve the generated Greet service for the Connect-ES interoperability test.

use futures::stream;
use invariant::{
    BoxResponseStream, Code, Response, Server, Status, projections::http::http_router,
};
use std::io::Write;
use std::sync::Arc;
use tonic::Request;

mod greet {
    include!(concat!(env!("OUT_DIR"), "/greet.v1.rs"));
}

struct GreetService;

#[tonic::async_trait]
impl greet::greet_service_server::GreetService for GreetService {
    async fn greet(
        &self,
        request: Request<greet::GreetRequest>,
    ) -> Result<Response<greet::GreetResponse>, Status> {
        let request = request.into_inner();
        if request.name == "error" {
            return Err(Status::new(Code::FailedPrecondition, "interop status"));
        }
        Ok(Response::new(greet::GreetResponse {
            message: format!("Hi {}", request.name),
            ..Default::default()
        }))
    }

    async fn greet_group(
        &self,
        request: Request<greet::GreetGroupRequest>,
    ) -> Result<Response<greet::GreetGroupResponse>, Status> {
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
    }

    type StreamGreetStream = BoxResponseStream<greet::GreetResponse>;

    async fn stream_greet(
        &self,
        request: Request<greet::StreamGreetRequest>,
    ) -> Result<Response<Self::StreamGreetStream>, Status> {
        let request = request.into_inner();
        let count = request.count.max(1);
        let name = request.name;
        let responses = stream::iter((0..count).map(move |index| {
            Ok(greet::GreetResponse {
                message: format!("Hi {name} #{index}"),
                ..Default::default()
            })
        }));
        Ok(Response::new(Box::pin(responses)))
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let descriptor = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../python/tests/proto/descriptor.binpb"
    );
    let server = Arc::new(Server::from_descriptor(descriptor)?);
    greet::register_greet_service_server(&server, GreetService)?;

    let app = http_router(server);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    println!("http://{}", listener.local_addr()?);
    std::io::stdout().flush()?;
    axum::serve(listener, app).await?;
    Ok(())
}
