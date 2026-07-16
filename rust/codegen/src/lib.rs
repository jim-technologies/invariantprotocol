//! Build-time generation of tonic services plus Invariant registration adapters.

use heck::ToSnakeCase;
use prost::Message;
use prost_build::{Method, Service, ServiceGenerator};
use prost_types::{FileDescriptorProto, FileDescriptorSet, field_descriptor_proto};
use std::collections::{BTreeMap, BTreeSet};
use std::io;
use std::path::{Path, PathBuf};

/// Create the conventional generator. Generated code uses `::invariant` as
/// the runtime crate path; override it only when the dependency is renamed.
pub fn configure() -> Builder {
    Builder {
        out_dir: None,
        runtime_path: "::invariant".to_string(),
    }
}

/// Minimal convention-first generator configuration.
pub struct Builder {
    out_dir: Option<PathBuf>,
    runtime_path: String,
}

impl Builder {
    pub fn out_dir(mut self, path: impl AsRef<Path>) -> Self {
        self.out_dir = Some(path.as_ref().to_path_buf());
        self
    }

    pub fn runtime_path(mut self, path: impl Into<String>) -> Self {
        self.runtime_path = path.into();
        self
    }

    /// Generate prost messages, normal tonic clients/servers, and one typed
    /// Invariant registration helper per service from this exact image.
    pub fn compile_fds(self, fds: FileDescriptorSet) -> io::Result<()> {
        let graphs = reachable_service_graphs(&fds);
        let tonic = tonic_prost_build::configure().service_generator();
        let adapter = AdapterGenerator {
            runtime_path: self.runtime_path,
            graphs,
        };
        let mut config = prost_build::Config::new();
        config.enable_type_names();
        if let Some(path) = self.out_dir {
            config.out_dir(path);
        }
        config.service_generator(Box::new(CombinedGenerator { tonic, adapter }));
        config.compile_fds(fds)
    }
}

struct CombinedGenerator {
    tonic: Box<dyn ServiceGenerator>,
    adapter: AdapterGenerator,
}

impl ServiceGenerator for CombinedGenerator {
    fn generate(&mut self, service: Service, output: &mut String) {
        self.tonic.generate(service.clone(), output);
        self.adapter.generate(service, output);
    }

    fn finalize(&mut self, output: &mut String) {
        self.tonic.finalize(output);
        self.adapter.finalize(output);
    }

    fn finalize_package(&mut self, package: &str, output: &mut String) {
        self.tonic.finalize_package(package, output);
        self.adapter.finalize_package(package, output);
    }
}

struct AdapterGenerator {
    runtime_path: String,
    graphs: BTreeMap<String, Vec<u8>>,
}

impl ServiceGenerator for AdapterGenerator {
    fn generate(&mut self, service: Service, output: &mut String) {
        let runtime = &self.runtime_path;
        let service_name = if service.package.is_empty() {
            service.proto_name.clone()
        } else {
            format!("{}.{}", service.package, service.proto_name)
        };
        let snake = service.proto_name.to_snake_case();
        let trait_name = &service.name;
        let server_name = format!("{}Server", service.name);
        let adapter_name = format!("Invariant{}Adapter", service.name);
        let helper_name = format!("register_{}_server", snake);
        let helper_with_name = format!("register_{}_server_with", snake);
        let graph = self
            .graphs
            .get(&service_name)
            .unwrap_or_else(|| panic!("missing descriptor graph for {service_name}"));
        let graph_literal = graph
            .iter()
            .map(u8::to_string)
            .collect::<Vec<_>>()
            .join(",");

        output.push_str(&format!(
            r#"
#[doc(hidden)]
#[derive(Clone)]
pub struct {adapter_name}<T: {snake}_server::{trait_name}> {{
    server: {runtime}::Server,
    implementation: ::std::sync::Arc<T>,
}}

#[::tonic::async_trait]
impl<T: {snake}_server::{trait_name}> {snake}_server::{trait_name} for {adapter_name}<T> {{
"#,
        ));
        for method in &service.methods {
            output.push_str(&adapter_method(runtime, &service_name, method));
        }
        output.push_str("}\n");

        output.push_str(&format!(
            r#"
/// Register one generated tonic implementation as the canonical native
/// service and project its unary/server-streaming methods through Invariant.
pub fn {helper_name}<T>(
    server: &{runtime}::Server,
    implementation: T,
) -> ::core::result::Result<(), ::tonic::Status>
where
    T: {snake}_server::{trait_name},
{{
    {helper_with_name}(server, implementation, |native| native)
}}

/// Register with conventional generated-tonic service configuration such as
/// compression and decoding/encoding message limits.
pub fn {helper_with_name}<T, F, S>(
    server: &{runtime}::Server,
    implementation: T,
    configure: F,
) -> ::core::result::Result<(), ::tonic::Status>
where
    T: {snake}_server::{trait_name},
    F: ::core::ops::FnOnce(
        {snake}_server::{server_name}<{adapter_name}<T>>,
    ) -> S,
    S: {runtime}::NativeService,
{{
    const DESCRIPTOR_GRAPH: &[u8] = &[{graph_literal}];
    let implementation = ::std::sync::Arc::new(implementation);
    let adapter = ::std::sync::Arc::new({adapter_name} {{
        server: server.clone(),
        implementation,
    }});
    let mut registration = {runtime}::ServiceRegistration::new(
        "{service_name}",
        DESCRIPTOR_GRAPH,
    );
"#,
        ));
        for method in &service.methods {
            if method.client_streaming {
                continue;
            }
            output.push_str(&projection_binding(&snake, trait_name, method));
        }
        output.push_str(&format!(
            r#"
    let native = configure({snake}_server::{server_name}::from_arc(adapter));
    server.register_generated_service(native, registration)
}}
"#,
        ));
    }
}

fn adapter_method(runtime: &str, service_name: &str, method: &Method) -> String {
    let name = &method.name;
    let input = &method.input_type;
    let output = &method.output_type;
    let info = format!(
        "{runtime}::ServerCallInfo::new(\"{service_name}\", \"{}\")",
        method.proto_name
    );
    match (method.client_streaming, method.server_streaming) {
        (false, false) => format!(
            r#"
    async fn {name}(
        &self,
        request: ::tonic::Request<{input}>,
    ) -> ::core::result::Result<::tonic::Response<{output}>, ::tonic::Status> {{
        let implementation = self.implementation.clone();
        self.server.invoke_typed_unary(request, {info}, move |request| {{
            let implementation = implementation.clone();
            async move {{ implementation.{name}(request).await }}
        }}).await
    }}
"#,
        ),
        (false, true) => {
            let associated = format!("{}Stream", method.proto_name);
            format!(
                r#"
    type {associated} = {runtime}::BoxResponseStream<{output}>;

    async fn {name}(
        &self,
        request: ::tonic::Request<{input}>,
    ) -> ::core::result::Result<::tonic::Response<Self::{associated}>, ::tonic::Status> {{
        let implementation = self.implementation.clone();
        self.server.invoke_typed_stream(request, {info}, move |request| {{
            let implementation = implementation.clone();
            async move {{
                let response = implementation.{name}(request).await?;
                Ok(response.map(|stream| Box::pin(stream) as {runtime}::BoxResponseStream<{output}>))
            }}
        }}).await
    }}
"#,
            )
        }
        (true, false) => format!(
            r#"
    async fn {name}(
        &self,
        request: ::tonic::Request<::tonic::Streaming<{input}>>,
    ) -> ::core::result::Result<::tonic::Response<{output}>, ::tonic::Status> {{
        let implementation = self.implementation.clone();
        self.server.invoke_typed_stream_call(request, {info}, move |request| {{
            let implementation = implementation.clone();
            async move {{ implementation.{name}(request).await }}
        }}).await
    }}
"#,
        ),
        (true, true) => {
            let associated = format!("{}Stream", method.proto_name);
            format!(
                r#"
    type {associated} = {runtime}::BoxResponseStream<{output}>;

    async fn {name}(
        &self,
        request: ::tonic::Request<::tonic::Streaming<{input}>>,
    ) -> ::core::result::Result<::tonic::Response<Self::{associated}>, ::tonic::Status> {{
        let implementation = self.implementation.clone();
        self.server.invoke_typed_stream_call(request, {info}, move |request| {{
            let implementation = implementation.clone();
            async move {{
                let response = implementation.{name}(request).await?;
                Ok(response.map(|stream| Box::pin(stream) as {runtime}::BoxResponseStream<{output}>))
            }}
        }}).await
    }}
"#,
            )
        }
    }
}

fn projection_binding(snake: &str, trait_name: &str, method: &Method) -> String {
    let method_name = &method.proto_name;
    let rust_name = &method.name;
    let input = &method.input_type;
    let output = &method.output_type;
    if method.server_streaming {
        format!(
            r#"
    {{
        let adapter = adapter.clone();
        registration.server_streaming::<{input}, {output}, _, _>(
            "{method_name}",
            move |request| {{
                let adapter = adapter.clone();
                async move {{ {snake}_server::{trait_name}::{rust_name}(&*adapter, request).await }}
            }},
        )?;
    }}
"#,
        )
    } else {
        format!(
            r#"
    {{
        let adapter = adapter.clone();
        registration.unary::<{input}, {output}, _, _>(
            "{method_name}",
            move |request| {{
                let adapter = adapter.clone();
                async move {{ {snake}_server::{trait_name}::{rust_name}(&*adapter, request).await }}
            }},
        )?;
    }}
"#,
        )
    }
}

fn reachable_service_graphs(fds: &FileDescriptorSet) -> BTreeMap<String, Vec<u8>> {
    let files = fds
        .file
        .iter()
        .filter_map(|file| file.name.as_ref().map(|name| (name.clone(), file)))
        .collect::<BTreeMap<_, _>>();
    let mut messages = BTreeMap::new();
    let mut enums = BTreeMap::new();
    for file in &fds.file {
        let package = file.package.as_deref().unwrap_or("");
        for message in &file.message_type {
            index_message(file, package, message, &mut messages, &mut enums);
        }
        for enumeration in &file.enum_type {
            enums.insert(qualified(package, enumeration.name()), file.name());
        }
    }

    let mut out = BTreeMap::new();
    for file in &fds.file {
        let package = file.package.as_deref().unwrap_or("");
        for service in &file.service {
            let service_name = qualified(package, service.name());
            let mut paths = BTreeSet::from([file.name().to_string()]);
            let mut seen = BTreeSet::new();
            for method in &service.method {
                add_message_files(
                    method.input_type().trim_start_matches('.'),
                    &messages,
                    &enums,
                    &mut paths,
                    &mut seen,
                );
                add_message_files(
                    method.output_type().trim_start_matches('.'),
                    &messages,
                    &enums,
                    &mut paths,
                    &mut seen,
                );
            }
            add_file_dependencies(&files, &mut paths);
            let mut graph = FileDescriptorSet::default();
            for path in paths {
                if let Some(file) = files.get(&path) {
                    let mut file = (*file).clone();
                    file.source_code_info = None;
                    graph.file.push(file);
                }
            }
            out.insert(service_name, graph.encode_to_vec());
        }
    }
    out
}

fn add_file_dependencies(
    files: &BTreeMap<String, &FileDescriptorProto>,
    paths: &mut BTreeSet<String>,
) {
    let mut pending = paths.iter().cloned().collect::<Vec<_>>();
    while let Some(path) = pending.pop() {
        let Some(file) = files.get(&path) else {
            continue;
        };
        for dependency in &file.dependency {
            if paths.insert(dependency.clone()) {
                pending.push(dependency.clone());
            }
        }
    }
}

type MessageIndex<'a> =
    BTreeMap<String, (&'a FileDescriptorProto, &'a prost_types::DescriptorProto)>;
type EnumIndex<'a> = BTreeMap<String, &'a str>;

fn index_message<'a>(
    file: &'a FileDescriptorProto,
    parent: &str,
    message: &'a prost_types::DescriptorProto,
    messages: &mut MessageIndex<'a>,
    enums: &mut EnumIndex<'a>,
) {
    let name = qualified(parent, message.name());
    messages.insert(name.clone(), (file, message));
    for nested in &message.nested_type {
        index_message(file, &name, nested, messages, enums);
    }
    for enumeration in &message.enum_type {
        enums.insert(qualified(&name, enumeration.name()), file.name());
    }
}

fn add_message_files(
    name: &str,
    messages: &MessageIndex<'_>,
    enums: &EnumIndex<'_>,
    paths: &mut BTreeSet<String>,
    seen: &mut BTreeSet<String>,
) {
    if !seen.insert(name.to_string()) {
        return;
    }
    let Some((file, message)) = messages.get(name) else {
        return;
    };
    paths.insert(file.name().to_string());
    for field in &message.field {
        let type_name = field.type_name().trim_start_matches('.');
        match field.r#type() {
            field_descriptor_proto::Type::Message | field_descriptor_proto::Type::Group => {
                add_message_files(type_name, messages, enums, paths, seen);
            }
            field_descriptor_proto::Type::Enum => {
                if let Some(path) = enums.get(type_name) {
                    paths.insert((*path).to_string());
                }
            }
            _ => {}
        }
    }
}

fn qualified(parent: &str, name: &str) -> String {
    if parent.is_empty() {
        name.to_string()
    } else {
        format!("{parent}.{name}")
    }
}
