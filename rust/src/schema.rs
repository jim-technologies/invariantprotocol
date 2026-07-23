//! JSON Schema generation from proto types. Mirrors `go/schema.go` and
//! `python/.../schema.py` field-by-field — same well-known type mappings,
//! same recursion + cycle guard, same field-type switch on numeric proto
//! type ids. Output is intentionally identical across the three impls so
//! a single client snapshot covers all of them.

use crate::descriptor::{FieldInfo, MessageInfo, ParsedDescriptor};
use serde_json::{Map, Value, json};
use std::collections::HashSet;

// `FieldDescriptorProto::Type` constants, lifted to keep the match readable.
const TYPE_DOUBLE: i32 = 1;
const TYPE_FLOAT: i32 = 2;
const TYPE_INT64: i32 = 3;
const TYPE_UINT64: i32 = 4;
const TYPE_INT32: i32 = 5;
const TYPE_FIXED64: i32 = 6;
const TYPE_FIXED32: i32 = 7;
const TYPE_BOOL: i32 = 8;
const TYPE_STRING: i32 = 9;
const TYPE_MESSAGE: i32 = 11;
const TYPE_BYTES: i32 = 12;
const TYPE_UINT32: i32 = 13;
const TYPE_ENUM: i32 = 14;
const TYPE_SFIXED32: i32 = 15;
const TYPE_SFIXED64: i32 = 16;
const TYPE_SINT32: i32 = 17;
const TYPE_SINT64: i32 = 18;
const LABEL_REPEATED: i32 = 3;

pub struct SchemaGen<'d> {
    parsed: &'d ParsedDescriptor,
}

impl<'d> SchemaGen<'d> {
    pub fn new(parsed: &'d ParsedDescriptor) -> Self {
        Self { parsed }
    }

    /// Convert a fully-qualified message name to a JSON Schema object.
    pub fn message_to_schema(&self, full_name: &str) -> Value {
        let Some(msg) = self.parsed.messages.get(full_name) else {
            return json!({"type": "object"});
        };
        let mut visiting = HashSet::new();
        self.message_schema(msg, &mut visiting)
    }

    fn message_schema(&self, msg: &MessageInfo, visiting: &mut HashSet<String>) -> Value {
        let mut properties = Map::new();
        let mut required: Vec<String> = Vec::new();

        let mut oneof_fields: HashSet<&str> = HashSet::new();
        for oneof in &msg.oneofs {
            for fname in &oneof.field_names {
                oneof_fields.insert(fname.as_str());
            }
        }

        for field in &msg.fields {
            let mut prop = if self.is_map_field(field) {
                self.map_schema(field, visiting)
            } else if field.label == LABEL_REPEATED {
                json!({
                    "type": "array",
                    "items": self.field_type_schema(field, visiting),
                })
            } else {
                self.field_type_schema(field, visiting)
            };

            if !field.comment.is_empty()
                && let Some(obj) = prop.as_object_mut()
            {
                obj.insert(
                    "description".to_string(),
                    Value::String(field.comment.clone()),
                );
            }

            properties.insert(field.json_name.clone(), prop);

            let is_in_oneof =
                oneof_fields.contains(field.name.as_str()) || field.oneof_index.is_some();
            if field.label != LABEL_REPEATED && !is_in_oneof && !field.optional {
                required.push(field.json_name.clone());
            }
        }

        let mut schema = Map::new();
        schema.insert("type".to_string(), Value::String("object".to_string()));
        schema.insert("properties".to_string(), Value::Object(properties));
        schema.insert("additionalProperties".to_string(), Value::Bool(false));
        if !required.is_empty() {
            schema.insert(
                "required".to_string(),
                Value::Array(required.into_iter().map(Value::String).collect()),
            );
        }
        Value::Object(schema)
    }

    fn field_type_schema(&self, field: &FieldInfo, visiting: &mut HashSet<String>) -> Value {
        match field.r#type {
            TYPE_DOUBLE | TYPE_FLOAT => number_or_non_finite_schema(),
            TYPE_INT32 | TYPE_SINT32 | TYPE_SFIXED32 => {
                json!({"type": "integer"})
            }
            TYPE_UINT32 | TYPE_FIXED32 => {
                json!({"type": "integer", "minimum": 0})
            }
            TYPE_INT64 | TYPE_SINT64 | TYPE_SFIXED64 => {
                json!({"type": "string", "pattern": "^(0|-?[1-9][0-9]*)$"})
            }
            TYPE_UINT64 | TYPE_FIXED64 => {
                json!({"type": "string", "pattern": "^(0|[1-9][0-9]*)$"})
            }
            TYPE_BOOL => json!({"type": "boolean"}),
            TYPE_STRING => json!({"type": "string"}),
            TYPE_BYTES => json!({"type": "string", "contentEncoding": "base64"}),
            TYPE_ENUM => self.enum_schema(&field.type_name),
            TYPE_MESSAGE => self.message_type_schema(&field.type_name, visiting),
            _ => Value::Object(Map::new()),
        }
    }

    fn message_type_schema(&self, type_name: &str, visiting: &mut HashSet<String>) -> Value {
        if let Some(wkt) = wkt_schema(type_name) {
            return wkt;
        }
        if visiting.contains(type_name) {
            return json!({"type": "object"});
        }
        let Some(msg) = self.parsed.messages.get(type_name) else {
            return json!({"type": "object"});
        };
        visiting.insert(type_name.to_string());
        let schema = self.message_schema(msg, visiting);
        visiting.remove(type_name);
        schema
    }

    fn enum_schema(&self, type_name: &str) -> Value {
        if type_name == "google.protobuf.NullValue" {
            return json!({"type": "null"});
        }
        let Some(info) = self.parsed.enums.get(type_name) else {
            return json!({"type": "string"});
        };
        let names: Vec<Value> = info
            .values
            .iter()
            .map(|v| Value::String(v.name.clone()))
            .collect();
        json!({"type": "string", "enum": names})
    }

    fn is_map_field(&self, field: &FieldInfo) -> bool {
        if field.label != LABEL_REPEATED || field.r#type != TYPE_MESSAGE {
            return false;
        }
        self.parsed
            .messages
            .get(&field.type_name)
            .is_some_and(|m| m.is_map_entry)
    }

    fn map_schema(&self, field: &FieldInfo, visiting: &mut HashSet<String>) -> Value {
        let Some(entry) = self.parsed.messages.get(&field.type_name) else {
            return json!({"type": "object"});
        };
        let value_field = entry.fields.iter().find(|f| f.name == "value");
        let Some(vf) = value_field else {
            return json!({"type": "object"});
        };
        let mut schema = json!({
            "type": "object",
            "additionalProperties": self.field_type_schema(vf, visiting),
        });
        if let Some(key_field) = entry.fields.iter().find(|f| f.name == "key")
            && let Some(property_names) = map_key_schema(key_field.r#type)
        {
            schema
                .as_object_mut()
                .expect("map schema is an object")
                .insert("propertyNames".to_string(), property_names);
        }
        schema
    }
}

fn map_key_schema(field_type: i32) -> Option<Value> {
    match field_type {
        TYPE_BOOL => Some(json!({"enum": ["false", "true"]})),
        TYPE_INT32 | TYPE_INT64 | TYPE_SINT32 | TYPE_SINT64 | TYPE_SFIXED32 | TYPE_SFIXED64 => {
            Some(json!({"pattern": "^(0|-?[1-9][0-9]*)$"}))
        }
        TYPE_UINT32 | TYPE_UINT64 | TYPE_FIXED32 | TYPE_FIXED64 => {
            Some(json!({"pattern": "^(0|[1-9][0-9]*)$"}))
        }
        _ => None,
    }
}

fn wkt_schema(type_name: &str) -> Option<Value> {
    match type_name {
        "google.protobuf.Timestamp" => Some(json!({"type": "string", "format": "date-time"})),
        "google.protobuf.Duration" => Some(json!({
            "type": "string",
            "pattern": "^-?(?:0|[1-9][0-9]*)(?:\\.[0-9]{1,9})?s$"
        })),
        "google.protobuf.Any" => Some(json!({"type": "object"})),
        "google.protobuf.Struct" => Some(json!({"type": "object"})),
        "google.protobuf.Value" => Some(Value::Object(Map::new())),
        "google.protobuf.DoubleValue" | "google.protobuf.FloatValue" => Some(json!({
            "oneOf": [
                {"type": "number"},
                {"type": "string", "enum": ["NaN", "Infinity", "-Infinity"]}
            ]
        })),
        "google.protobuf.Int64Value" => {
            Some(json!({"type": "string", "pattern": "^(0|-?[1-9][0-9]*)$"}))
        }
        "google.protobuf.Int32Value" => Some(json!({"type": "integer"})),
        "google.protobuf.UInt64Value" => {
            Some(json!({"type": "string", "pattern": "^(0|[1-9][0-9]*)$"}))
        }
        "google.protobuf.UInt32Value" => Some(json!({"type": "integer", "minimum": 0})),
        "google.protobuf.BoolValue" => Some(json!({"type": "boolean"})),
        "google.protobuf.StringValue" => Some(json!({"type": "string"})),
        "google.protobuf.BytesValue" => {
            Some(json!({"type": "string", "contentEncoding": "base64"}))
        }
        "google.protobuf.FieldMask" => Some(json!({"type": "string"})),
        "google.protobuf.ListValue" => Some(json!({"type": "array", "items": {}})),
        "google.protobuf.Empty" => Some(json!({"type": "object", "additionalProperties": false})),
        _ => None,
    }
}

fn number_or_non_finite_schema() -> Value {
    json!({
        "oneOf": [
            {"type": "number"},
            {"type": "string", "enum": ["NaN", "Infinity", "-Infinity"]}
        ]
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn protojson_special_types_and_map_keys_have_wire_accurate_schemas() {
        assert_eq!(
            number_or_non_finite_schema(),
            json!({
                "oneOf": [
                    {"type": "number"},
                    {"type": "string", "enum": ["NaN", "Infinity", "-Infinity"]}
                ]
            })
        );
        assert_eq!(
            wkt_schema("google.protobuf.Duration"),
            Some(json!({
                "type": "string",
                "pattern": "^-?(?:0|[1-9][0-9]*)(?:\\.[0-9]{1,9})?s$"
            }))
        );
        assert_eq!(
            wkt_schema("google.protobuf.FieldMask"),
            Some(json!({"type": "string"}))
        );
        assert_eq!(
            wkt_schema("google.protobuf.ListValue"),
            Some(json!({"type": "array", "items": {}}))
        );
        assert_eq!(
            wkt_schema("google.protobuf.Empty"),
            Some(json!({"type": "object", "additionalProperties": false}))
        );
        assert_eq!(
            map_key_schema(TYPE_BOOL),
            Some(json!({"enum": ["false", "true"]}))
        );
        assert_eq!(
            map_key_schema(TYPE_INT64),
            Some(json!({"pattern": "^(0|-?[1-9][0-9]*)$"}))
        );
        assert_eq!(
            map_key_schema(TYPE_UINT64),
            Some(json!({"pattern": "^(0|[1-9][0-9]*)$"}))
        );
        assert_eq!(map_key_schema(TYPE_STRING), None);
    }
}
