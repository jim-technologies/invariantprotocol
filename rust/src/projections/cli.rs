//! CLI projection — `ServiceName Method [-r request]`. Writes either a single
//! pretty-printed JSON response (unary) or one compact JSON line per chunk
//! (streaming). Mirrors `go/cli.go` and `python/.../cli.py`.

use crate::server::Server;
use futures::StreamExt;
use prost_reflect::{DynamicMessage, SerializeOptions};
use std::sync::Arc;
use tokio::io::AsyncWriteExt;
use tonic::{Request as GrpcRequest, Status};

/// Run a single CLI invocation, writing output to `out`. For streaming tools
/// each chunk is flushed immediately — same UX as the Go/Python projections.
pub async fn cli_write<W>(server: Arc<Server>, args: &[String], out: &mut W) -> Result<(), Status>
where
    W: AsyncWriteExt + Unpin,
{
    server.freeze();
    if args.is_empty() || matches!(args[0].as_str(), "-h" | "--help") {
        let help = cli_help(&server);
        out.write_all(help.as_bytes())
            .await
            .map_err(|error| Status::internal(format!("write output: {error}")))?;
        return Ok(());
    }

    let (service, method, request_value) = split_args(args)?;
    let tool_name = resolve_tool(&server, &service, &method)?;
    let tool = server
        .tool(&tool_name)
        .ok_or_else(|| Status::not_found(format!("unknown tool: {tool_name}")))?;

    let req = build_request(&tool, request_value.as_deref())?;

    if tool.server_streaming {
        let mut stream = server
            .invoke_stream(&tool_name, GrpcRequest::new(req))
            .await?
            .into_inner();
        while let Some(item) = stream.next().await {
            let msg = item?;
            let line = serialize_compact(&msg);
            out.write_all(line.as_bytes())
                .await
                .map_err(|error| Status::internal(format!("write output: {error}")))?;
            out.write_all(b"\n")
                .await
                .map_err(|error| Status::internal(format!("write output: {error}")))?;
            out.flush()
                .await
                .map_err(|error| Status::internal(format!("flush output: {error}")))?;
        }
        return Ok(());
    }

    let resp = server
        .invoke(&tool_name, GrpcRequest::new(req))
        .await?
        .into_inner();
    let text = serialize_pretty(&resp);
    out.write_all(text.as_bytes())
        .await
        .map_err(|error| Status::internal(format!("write output: {error}")))?;
    Ok(())
}

fn split_args(args: &[String]) -> Result<(String, String, Option<String>), Status> {
    if args.is_empty() || args[0].starts_with('-') {
        return Err(Status::invalid_argument(
            "expected ServiceName as first argument",
        ));
    }
    let service = args[0].clone();
    if args.len() < 2 || args[1].starts_with('-') {
        return Err(Status::invalid_argument(
            "expected Method name after ServiceName",
        ));
    }
    let method = args[1].clone();
    let mut request_value = None;
    let mut i = 2;
    while i < args.len() {
        match args[i].as_str() {
            "-r" => {
                if i + 1 >= args.len() {
                    return Err(Status::invalid_argument("missing value after -r"));
                }
                request_value = Some(args[i + 1].clone());
                i += 2;
            }
            other => {
                return Err(Status::invalid_argument(format!("unexpected arg: {other}")));
            }
        }
    }
    Ok((service, method, request_value))
}

fn resolve_tool(server: &Server, service: &str, method: &str) -> Result<String, Status> {
    for tool in server.tools_snapshot() {
        let svc = tool
            .service_full_name
            .rsplit('.')
            .next()
            .unwrap_or(&tool.service_full_name);
        if svc == service && tool.method_name == method {
            return Ok(tool.name.clone());
        }
    }
    Err(Status::not_found(format!(
        "unknown service/method: {service} {method}"
    )))
}

fn build_request(
    tool: &Arc<crate::server::Tool>,
    value: Option<&str>,
) -> Result<DynamicMessage, Status> {
    let Some(value) = value else {
        return Ok(DynamicMessage::new(tool.input_desc.clone()));
    };
    let bytes: Vec<u8> = if std::path::Path::new(value).is_file() {
        let ext = std::path::Path::new(value)
            .extension()
            .and_then(|e| e.to_str())
            .map(|s| s.to_lowercase());
        let data = std::fs::read(value)
            .map_err(|e| Status::invalid_argument(format!("read {value}: {e}")))?;
        match ext.as_deref() {
            Some("binpb") | Some("pb") => {
                return DynamicMessage::decode(tool.input_desc.clone(), &data[..])
                    .map_err(|e| Status::invalid_argument(format!("decode binary proto: {e}")));
            }
            Some("json") | None => data,
            Some(ext) => {
                return Err(Status::invalid_argument(format!(
                    "unsupported request file extension {ext:?} (use .json, .binpb, or .pb)"
                )));
            }
        }
    } else {
        value.as_bytes().to_vec()
    };
    let mut deserializer = serde_json::Deserializer::from_slice(&bytes);
    let opts = prost_reflect::DeserializeOptions::new();
    DynamicMessage::deserialize_with_options(tool.input_desc.clone(), &mut deserializer, &opts)
        .map_err(|e| Status::invalid_argument(format!("proto: {e}")))
}

fn serialize_pretty(msg: &DynamicMessage) -> String {
    let opts = SerializeOptions::new().use_proto_field_name(true);
    let mut buf = Vec::with_capacity(128);
    let mut ser = serde_json::Serializer::pretty(&mut buf);
    let _ = msg.serialize_with_options(&mut ser, &opts);
    String::from_utf8(buf).unwrap_or_default()
}

fn serialize_compact(msg: &DynamicMessage) -> String {
    let opts = SerializeOptions::new().use_proto_field_name(true);
    let mut buf = Vec::with_capacity(128);
    let mut ser = serde_json::Serializer::new(&mut buf);
    let _ = msg.serialize_with_options(&mut ser, &opts);
    String::from_utf8(buf).unwrap_or_default()
}

fn cli_help(server: &Server) -> String {
    let mut out = String::new();
    out.push_str("Usage: <binary> <ServiceName> <Method> [-r request.json|'{json}']\n\n");
    let tools = server.tools_snapshot();
    if tools.is_empty() {
        out.push_str("No tools registered.\n");
        return out;
    }
    out.push_str("Available methods:\n\n");
    for tool in tools {
        let svc = tool
            .service_full_name
            .rsplit('.')
            .next()
            .unwrap_or(&tool.service_full_name);
        out.push_str(&format!("  {svc} {}\n", tool.method_name));
        if !tool.description.is_empty() && tool.description != tool.name {
            out.push_str(&format!("    {}\n", tool.description));
        }
    }
    out
}
