//! Server-streaming primitives.
//!
//! Same shape as Go's `ServerStream` interface and Python's async-gen
//! handlers: the user pushes responses; the framework owns the consumer side.
//! Rust idiom: a typed sender + tokio mpsc, plus a `BoxStream` for the
//! projection-side receiver.

use crate::errors::Status;
use futures::stream::BoxStream;
use prost::Message;
use prost_reflect::{DynamicMessage, MessageDescriptor};
use std::marker::PhantomData;
use std::sync::Arc;
use tokio::sync::mpsc;

/// Typed sender handed to user streaming handlers. Mirrors gRPC's typed
/// ServerStream `Send` method shape.
pub struct ServerStreamTx<Resp: Message> {
    inner: Arc<DynStreamTx>,
    _resp: PhantomData<fn(Resp)>,
}

impl<Resp: Message> Clone for ServerStreamTx<Resp> {
    fn clone(&self) -> Self {
        Self {
            inner: self.inner.clone(),
            _resp: PhantomData,
        }
    }
}

impl<Resp: Message> ServerStreamTx<Resp> {
    pub(crate) fn new(inner: Arc<DynStreamTx>) -> Self {
        Self {
            inner,
            _resp: PhantomData,
        }
    }

    /// Send one response message. Returns an error if the receiver has been
    /// dropped (client disconnect, deadline elapsed, server cancellation).
    pub async fn send(&self, resp: Resp) -> Result<(), Status> {
        let buf = resp.encode_to_vec();
        let dyn_msg = DynamicMessage::decode(self.inner.output_desc.clone(), &buf[..])
            .map_err(|e| Status::internal(format!("encode stream message: {e}")))?;
        self.inner.send(dyn_msg).await
    }
}

/// Type-erased stream sender used inside the framework. Each projection
/// constructs one of these, hands it to `invoke_stream`, and consumes the
/// receiver on its own side.
pub struct DynStreamTx {
    sender: mpsc::Sender<DynamicMessage>,
    pub(crate) output_desc: MessageDescriptor,
}

impl DynStreamTx {
    pub(crate) fn new(
        sender: mpsc::Sender<DynamicMessage>,
        output_desc: MessageDescriptor,
    ) -> Self {
        Self {
            sender,
            output_desc,
        }
    }

    pub(crate) async fn send(&self, msg: DynamicMessage) -> Result<(), Status> {
        self.sender
            .send(msg)
            .await
            .map_err(|_| Status::new(crate::errors::Code::Cancelled, "stream consumer dropped"))
    }
}

/// Convenience: create a paired (DynStreamTx, BoxStream) for a projection to
/// drive. Channel capacity is small — backpressure naturally throttles the
/// handler when the projection can't keep up.
pub(crate) fn dyn_stream_channel(
    output_desc: MessageDescriptor,
    capacity: usize,
) -> (Arc<DynStreamTx>, BoxStream<'static, DynamicMessage>) {
    use futures::stream::StreamExt;
    let (tx, rx) = mpsc::channel(capacity.max(1));
    let stream = tokio_stream::wrappers::ReceiverStream::new(rx).boxed();
    (Arc::new(DynStreamTx::new(tx, output_desc)), stream)
}

/// Default channel buffer. Matches Go's per-projection buffer size; tuned to
/// be big enough that a slow consumer doesn't immediately stall the handler
/// but small enough to bound memory usage on bursty streams.
pub(crate) const STREAM_BUFFER: usize = 32;
