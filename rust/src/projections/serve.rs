//! Optional-projection runner for HTTP/Connect and MCP stdio. Native gRPC
//! listener ownership and graceful shutdown stay with the caller through
//! `Server::grpc_routes()` and normal Tonic transport APIs.
//!
//! Cancellation propagates: when any projection returns (error or stdin EOF
//! for MCP) or the supplied cancellation token fires, all projections are
//! signalled to shut down gracefully. Same semantics as Go's `errc <- ...`.

use crate::projections::{http, mcp};
use crate::server::Server;
use futures::StreamExt;
use std::sync::Arc;
use tokio_util::sync::CancellationToken;

/// One projection to run.
#[derive(Debug, Clone, Copy)]
pub enum Projection {
    /// HTTP / Connect server on the given port. Also serves `/mcp` (MCP
    /// Streamable HTTP transport), `/healthz`, `/readyz`, the tool catalog,
    /// and the raw descriptor.
    Http(u16),
    /// MCP stdio transport. Blocks until stdin closes.
    McpStdio,
}

/// Errors a projection can return.
#[derive(Debug, thiserror::Error)]
pub enum ServeError {
    #[error("http: {0}")]
    Http(#[from] std::io::Error),
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

    let shutdown = CancellationToken::new();
    let mut handles = futures::stream::FuturesUnordered::new();
    for projection in projections {
        let server = server.clone();
        let projection_shutdown = shutdown.child_token();
        handles.push(tokio::spawn(run_projection(
            server,
            projection,
            projection_shutdown,
        )));
    }

    let first_result = tokio::select! {
        biased;
        _ = cancel.cancelled() => Ok(()),
        done = handles.next() => {
            match done {
                Some(Ok(result)) => result,
                Some(Err(join)) => Err(ServeError::Http(std::io::Error::other(format!(
                    "join: {join}"
                )))),
                None => Ok(()),
            }
        }
    };

    shutdown.cancel();
    while handles.next().await.is_some() {
        // Every projection owns its shutdown path; drain all tasks before
        // returning so no transport or in-flight call is detached.
    }
    first_result
}

async fn run_projection(
    server: Arc<Server>,
    projection: Projection,
    shutdown: CancellationToken,
) -> Result<(), ServeError> {
    match projection {
        Projection::Http(port) => {
            let app = http::http_router(server);
            let listener = tokio::net::TcpListener::bind(("0.0.0.0", port)).await?;
            axum::serve(listener, app)
                .with_graceful_shutdown(shutdown.cancelled_owned())
                .await
                .map_err(ServeError::Http)
        }
        Projection::McpStdio => mcp::serve_mcp_stdio_until_cancelled(server, shutdown)
            .await
            .map_err(ServeError::Http),
    }
}
