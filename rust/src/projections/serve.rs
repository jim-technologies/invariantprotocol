//! Multi-projection runner — mirrors Go's `Server.Serve(ctx, projections...)`
//! and Python's `await server.serve(http=..., grpc=..., mcp=True)`.
//!
//! Cancellation propagates: when any projection returns (error or stdin EOF
//! for MCP) or the supplied cancellation token fires, all projections are
//! signalled to shut down gracefully. Same semantics as Go's `errc <- ...`.

use crate::projections::{http, grpc, mcp};
use crate::server::Server;
use std::sync::Arc;
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

/// One projection to run.
#[derive(Debug, Clone, Copy)]
pub enum Projection {
    /// HTTP / Connect server on the given port. Also serves `/mcp` (MCP
    /// Streamable HTTP transport), `/healthz`, `/readyz`, the tool catalog,
    /// and the raw descriptor.
    Http(u16),
    /// gRPC server on the given port. Reflection auto-registered.
    Grpc(u16),
    /// MCP stdio transport. Blocks until stdin closes.
    McpStdio,
}

/// Errors a projection can return.
#[derive(Debug, thiserror::Error)]
pub enum ServeError {
    #[error("http: {0}")]
    Http(#[from] std::io::Error),
    #[error("grpc: {0}")]
    Grpc(#[from] tonic::transport::Error),
}

/// Serve one or more projections in parallel. Returns when the first
/// projection completes (error or otherwise) or `cancel` fires. The other
/// projections receive a graceful shutdown signal and are awaited.
///
/// Pass [`CancellationToken::new()`] if you don't need external cancellation
/// (the function still returns when the first projection completes).
pub async fn serve(
    server: Arc<Server>,
    projections: impl IntoIterator<Item = Projection>,
    cancel: CancellationToken,
) -> Result<(), ServeError> {
    let projections: Vec<_> = projections.into_iter().collect();
    if projections.is_empty() {
        return Ok(());
    }

    let (tx, rx) = watch::channel(false);
    let mut handles: Vec<tokio::task::JoinHandle<Result<(), ServeError>>> = Vec::new();

    for projection in projections {
        let server = server.clone();
        let mut shutdown = rx.clone();
        let handle = tokio::spawn(async move {
            tokio::select! {
                result = run_projection(server, projection) => result,
                _ = shutdown.changed() => Ok(()),
            }
        });
        handles.push(handle);
    }

    let cancel_fut = async {
        cancel.cancelled().await;
    };
    tokio::pin!(cancel_fut);

    // Wait for either the cancellation token or the first projection to finish.
    let first_result: Result<(), ServeError>;
    {
        let mut futures: futures::stream::FuturesUnordered<_> =
            handles.drain(..).collect();
        tokio::select! {
            biased;
            _ = &mut cancel_fut => {
                first_result = Ok(());
            }
            done = futures::StreamExt::next(&mut futures) => {
                first_result = match done {
                    Some(Ok(r)) => r,
                    Some(Err(join)) => Err(ServeError::Http(std::io::Error::other(format!("join: {join}")))),
                    None => Ok(()),
                };
            }
        }
        handles = futures.into_iter().collect();
    }

    // Signal the remainder and drain them.
    let _ = tx.send(true);
    for h in handles {
        let _ = h.await;
    }
    first_result
}

async fn run_projection(
    server: Arc<Server>,
    projection: Projection,
) -> Result<(), ServeError> {
    match projection {
        Projection::Http(port) => http::serve_http(server, port).await.map_err(ServeError::Http),
        Projection::Grpc(port) => grpc::serve_grpc(server, port).await.map_err(ServeError::Grpc),
        Projection::McpStdio => mcp::serve_mcp_stdio(server).await.map_err(ServeError::Http),
    }
}
