//! Parse `FileDescriptorSet` bytes into the framework's typed view of
//! services / methods / messages / enums, with source-info comments
//! attached. Mirrors `go/descriptor.go` and `python/.../descriptor.py`.
//!
//! `prost-reflect`'s `DescriptorPool` already knows everything about the
//! types — we only need the parallel structure to (a) attach comments
//! that `prost-reflect` doesn't surface, and (b) drive registration with
//! the same tool-name → method-info lookup the Go/Python versions use.

use crate::errors::Status;
use prost::Message;
use prost_reflect::DescriptorPool;
use prost_types::{
    DescriptorProto, EnumDescriptorProto, FileDescriptorProto, FileDescriptorSet,
    ServiceDescriptorProto,
};
use std::collections::BTreeMap;

#[derive(Debug, Clone)]
pub struct MethodInfo {
    pub name: String,
    pub input_type: String,
    pub output_type: String,
    pub comment: String,
    pub client_streaming: bool,
    pub server_streaming: bool,
}

#[derive(Debug, Clone)]
pub struct ServiceInfo {
    pub name: String,
    pub full_name: String,
    pub comment: String,
    /// Methods keyed by their simple name, insertion-ordered by descriptor.
    pub methods: BTreeMap<String, MethodInfo>,
}

#[derive(Debug, Clone)]
pub struct EnumValueInfo {
    pub name: String,
    pub number: i32,
    pub comment: String,
}

#[derive(Debug, Clone)]
pub struct EnumInfo {
    pub name: String,
    pub full_name: String,
    pub values: Vec<EnumValueInfo>,
    pub comment: String,
}

#[derive(Debug, Clone)]
pub struct FieldInfo {
    pub name: String,
    pub number: i32,
    /// `FieldDescriptorProto::Type` as i32 — Go/Python keep raw ints so the
    /// schema generator can use the same numeric switch.
    pub r#type: i32,
    pub type_name: String,
    /// `FieldDescriptorProto::Label` as i32 (`OPTIONAL=1`, `REQUIRED=2`, `REPEATED=3`).
    pub label: i32,
    pub comment: String,
    pub oneof_index: Option<i32>,
    /// True iff the field uses `optional` (proto3 explicit presence).
    pub proto3_optional: bool,
}

#[derive(Debug, Clone)]
pub struct OneofInfo {
    pub name: String,
    pub comment: String,
    pub field_names: Vec<String>,
}

#[derive(Debug, Clone)]
pub struct MessageInfo {
    pub name: String,
    pub full_name: String,
    pub fields: Vec<FieldInfo>,
    pub oneofs: Vec<OneofInfo>,
    pub comment: String,
    pub is_map_entry: bool,
}

/// Parsed view of a `FileDescriptorSet`, plus the `prost-reflect` pool
/// for dynamic message construction at wire boundaries.
pub struct ParsedDescriptor {
    pub services: BTreeMap<String, ServiceInfo>,
    pub messages: BTreeMap<String, MessageInfo>,
    pub enums: BTreeMap<String, EnumInfo>,
    pub pool: DescriptorPool,
    /// Original bytes so projections can re-serve `descriptor.binpb`.
    pub raw_fds: Vec<u8>,
}

impl ParsedDescriptor {
    pub fn from_bytes(data: &[u8]) -> Result<Self, Status> {
        let fds = FileDescriptorSet::decode(data)
            .map_err(|e| Status::invalid_argument(format!("unmarshal FileDescriptorSet: {e}")))?;
        let pool = DescriptorPool::decode(data)
            .map_err(|e| Status::invalid_argument(format!("build descriptor pool: {e}")))?;

        let mut out = Self {
            services: BTreeMap::new(),
            messages: BTreeMap::new(),
            enums: BTreeMap::new(),
            pool,
            raw_fds: data.to_vec(),
        };
        for file in fds.file {
            out.parse_file(&file);
        }
        Ok(out)
    }

    pub fn from_file(path: &str) -> Result<Self, Status> {
        let data = std::fs::read(path)
            .map_err(|e| Status::invalid_argument(format!("read descriptor file: {e}")))?;
        Self::from_bytes(&data)
    }

    fn parse_file(&mut self, file: &FileDescriptorProto) {
        let comments = extract_comments(file);
        let pkg = file.package.clone().unwrap_or_default();

        for (i, enum_proto) in file.enum_type.iter().enumerate() {
            let full = qualified_name(&pkg, enum_proto.name());
            let info = parse_enum(enum_proto, &full, &comments, &[5, i as i32]);
            self.enums.insert(full, info);
        }

        for (i, msg_proto) in file.message_type.iter().enumerate() {
            let full = qualified_name(&pkg, msg_proto.name());
            self.parse_message(msg_proto, &full, &comments, &[4, i as i32]);
        }

        for (i, svc_proto) in file.service.iter().enumerate() {
            let full = qualified_name(&pkg, svc_proto.name());
            let info = parse_service(svc_proto, &full, &comments, i as i32);
            self.services.insert(full, info);
        }
    }

    fn parse_message(
        &mut self,
        msg_proto: &DescriptorProto,
        full_name: &str,
        comments: &Comments,
        prefix: &[i32],
    ) {
        // Nested enums.
        for (i, e) in msg_proto.enum_type.iter().enumerate() {
            let nested = format!("{full_name}.{}", e.name());
            let mut path = prefix.to_vec();
            path.extend_from_slice(&[4, i as i32]);
            self.enums
                .insert(nested.clone(), parse_enum(e, &nested, comments, &path));
        }
        // Nested messages.
        for (i, m) in msg_proto.nested_type.iter().enumerate() {
            let nested = format!("{full_name}.{}", m.name());
            let mut path = prefix.to_vec();
            path.extend_from_slice(&[3, i as i32]);
            self.parse_message(m, &nested, comments, &path);
        }

        let mut oneofs: Vec<OneofInfo> = msg_proto
            .oneof_decl
            .iter()
            .enumerate()
            .map(|(i, o)| {
                let mut path = prefix.to_vec();
                path.extend_from_slice(&[8, i as i32]);
                OneofInfo {
                    name: o.name().to_string(),
                    comment: comments.get(&path).cloned().unwrap_or_default(),
                    field_names: Vec::new(),
                }
            })
            .collect();

        let mut fields = Vec::with_capacity(msg_proto.field.len());
        for (i, f) in msg_proto.field.iter().enumerate() {
            let mut path = prefix.to_vec();
            path.extend_from_slice(&[2, i as i32]);
            let comment = comments.get(&path).cloned().unwrap_or_default();
            let proto3_opt = f.proto3_optional.unwrap_or(false);
            let oneof_index = if proto3_opt { None } else { f.oneof_index };
            let type_name = f
                .type_name
                .clone()
                .unwrap_or_default()
                .trim_start_matches('.')
                .to_string();

            let field = FieldInfo {
                name: f.name().to_string(),
                number: f.number.unwrap_or(0),
                r#type: f.r#type.unwrap_or(0),
                type_name,
                label: f.label.unwrap_or(0),
                comment,
                oneof_index,
                proto3_optional: proto3_opt,
            };

            if let Some(idx) = oneof_index
                && let Some(slot) = oneofs.get_mut(idx as usize)
            {
                slot.field_names.push(field.name.clone());
            }
            fields.push(field);
        }

        let is_map_entry = msg_proto
            .options
            .as_ref()
            .and_then(|o| o.map_entry)
            .unwrap_or(false);

        self.messages.insert(
            full_name.to_string(),
            MessageInfo {
                name: msg_proto.name().to_string(),
                full_name: full_name.to_string(),
                fields,
                oneofs,
                comment: comments.get(prefix).cloned().unwrap_or_default(),
                is_map_entry,
            },
        );
    }
}

fn parse_enum(
    enum_proto: &EnumDescriptorProto,
    full_name: &str,
    comments: &Comments,
    prefix: &[i32],
) -> EnumInfo {
    let values = enum_proto
        .value
        .iter()
        .enumerate()
        .map(|(i, v)| {
            let mut path = prefix.to_vec();
            path.extend_from_slice(&[2, i as i32]);
            EnumValueInfo {
                name: v.name().to_string(),
                number: v.number.unwrap_or(0),
                comment: comments.get(&path).cloned().unwrap_or_default(),
            }
        })
        .collect();
    EnumInfo {
        name: enum_proto.name().to_string(),
        full_name: full_name.to_string(),
        values,
        comment: comments.get(prefix).cloned().unwrap_or_default(),
    }
}

fn parse_service(
    svc_proto: &ServiceDescriptorProto,
    full_name: &str,
    comments: &Comments,
    svc_index: i32,
) -> ServiceInfo {
    let mut methods = BTreeMap::new();
    for (j, m) in svc_proto.method.iter().enumerate() {
        let path = vec![6, svc_index, 2, j as i32];
        let input_type = m
            .input_type
            .clone()
            .unwrap_or_default()
            .trim_start_matches('.')
            .to_string();
        let output_type = m
            .output_type
            .clone()
            .unwrap_or_default()
            .trim_start_matches('.')
            .to_string();
        methods.insert(
            m.name().to_string(),
            MethodInfo {
                name: m.name().to_string(),
                input_type,
                output_type,
                comment: comments.get(&path).cloned().unwrap_or_default(),
                client_streaming: m.client_streaming.unwrap_or(false),
                server_streaming: m.server_streaming.unwrap_or(false),
            },
        );
    }
    ServiceInfo {
        name: svc_proto.name().to_string(),
        full_name: full_name.to_string(),
        comment: comments
            .get(&vec![6, svc_index])
            .cloned()
            .unwrap_or_default(),
        methods,
    }
}

type Comments = std::collections::HashMap<Vec<i32>, String>;

fn extract_comments(file: &FileDescriptorProto) -> Comments {
    let mut out = Comments::new();
    if let Some(sci) = &file.source_code_info {
        for loc in &sci.location {
            let comment = loc
                .leading_comments
                .clone()
                .filter(|s| !s.trim().is_empty())
                .or_else(|| loc.trailing_comments.clone())
                .unwrap_or_default()
                .trim()
                .to_string();
            if !comment.is_empty() {
                out.insert(loc.path.clone(), comment);
            }
        }
    }
    out
}

fn qualified_name(pkg: &str, name: &str) -> String {
    if pkg.is_empty() {
        name.to_string()
    } else {
        format!("{pkg}.{name}")
    }
}
