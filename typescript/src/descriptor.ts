import { readFileSync } from "node:fs";

import {
  createFileRegistry,
  type DescEnum,
  type DescField,
  type DescMessage,
  type DescMethod,
  type DescService,
  type FileRegistry,
  fromBinary,
} from "@bufbuild/protobuf";
import {
  type DescriptorProto,
  type EnumDescriptorProto,
  type FileDescriptorSet,
  FileDescriptorSetSchema,
} from "@bufbuild/protobuf/wkt";

export type MethodInfo = {
  name: string;
  inputType: string;
  outputType: string;
  comment: string;
  clientStreaming: boolean;
  serverStreaming: boolean;
  desc: DescMethod;
};

export type ServiceInfo = {
  name: string;
  fullName: string;
  comment: string;
  methods: Map<string, MethodInfo>;
  desc: DescService;
};

export class ParsedDescriptor {
  readonly fds: FileDescriptorSet;
  readonly bytes: Uint8Array;
  readonly registry: FileRegistry;
  readonly services = new Map<string, ServiceInfo>();
  readonly messages = new Map<string, DescMessage>();
  readonly enums = new Map<string, DescEnum>();
  readonly comments = new Map<string, string>();

  private constructor(fds: FileDescriptorSet, bytes: Uint8Array) {
    this.fds = fds;
    this.bytes = bytes;
    this.registry = createFileRegistry(fds);
    this.comments = collectComments(fds);

    for (const desc of this.registry) {
      if (desc.kind === "message") {
        this.messages.set(desc.typeName, desc);
      } else if (desc.kind === "enum") {
        this.enums.set(desc.typeName, desc);
      } else if (desc.kind === "service") {
        const methods = new Map<string, MethodInfo>();
        for (const method of desc.methods) {
          methods.set(method.name, {
            name: method.name,
            inputType: method.input.typeName,
            outputType: method.output.typeName,
            comment: this.commentForMethod(desc.typeName, method.name),
            clientStreaming: method.methodKind === "client_streaming" || method.methodKind === "bidi_streaming",
            serverStreaming: method.methodKind === "server_streaming" || method.methodKind === "bidi_streaming",
            desc: method,
          });
        }
        this.services.set(desc.typeName, {
          name: desc.name,
          fullName: desc.typeName,
          comment: this.commentFor(desc.typeName),
          methods,
          desc,
        });
      }
    }
  }

  static fromFile(path: string): ParsedDescriptor {
    return ParsedDescriptor.fromBytes(readFileSync(path));
  }

  static fromBytes(bytes: Uint8Array): ParsedDescriptor {
    const fds = fromBinary(FileDescriptorSetSchema, bytes);
    return new ParsedDescriptor(fds, bytes);
  }

  getMessage(typeName: string): DescMessage | undefined {
    return this.messages.get(stripLeadingDot(typeName));
  }

  getEnum(typeName: string): DescEnum | undefined {
    return this.enums.get(stripLeadingDot(typeName));
  }

  commentFor(symbol: string): string {
    return this.comments.get(stripLeadingDot(symbol)) ?? "";
  }

  commentForField(messageType: string, field: DescField | string): string {
    const fieldName = typeof field === "string" ? field : field.name;
    return this.commentFor(`${stripLeadingDot(messageType)}.${fieldName}`);
  }

  commentForMethod(serviceType: string, methodName: string): string {
    return this.commentFor(`${stripLeadingDot(serviceType)}.${methodName}`);
  }
}

function collectComments(fds: FileDescriptorSet): Map<string, string> {
  const out = new Map<string, string>();

  for (const file of fds.file) {
    const pkg = file.package;
    const locations = new Map<string, string>();
    for (const location of file.sourceCodeInfo?.location ?? []) {
      const comment = cleanComment(location.leadingComments || location.trailingComments);
      if (comment) {
        locations.set(location.path.join("."), comment);
      }
    }

    file.messageType.forEach((msg, i) => {
      collectMessageComments(out, locations, pkg, msg, [4, i], msg.name);
    });
    file.enumType.forEach((en, i) => {
      collectEnumComments(out, locations, pkg, en, [5, i], en.name);
    });
    file.service.forEach((svc, i) => {
      const serviceFull = joinName(pkg, svc.name);
      setComment(out, serviceFull, locations, [6, i]);
      svc.method.forEach((method, j) => {
        setComment(out, `${serviceFull}.${method.name}`, locations, [6, i, 2, j]);
      });
    });
  }

  return out;
}

function collectMessageComments(
  out: Map<string, string>,
  locations: Map<string, string>,
  pkg: string,
  msg: DescriptorProto,
  path: number[],
  localName: string,
): void {
  const full = joinName(pkg, localName);
  setComment(out, full, locations, path);

  msg.field.forEach((field, i) => {
    setComment(out, `${full}.${field.name}`, locations, [...path, 2, i]);
  });
  msg.nestedType.forEach((nested, i) => {
    collectMessageComments(out, locations, pkg, nested, [...path, 3, i], `${localName}.${nested.name}`);
  });
  msg.enumType.forEach((en, i) => {
    collectEnumComments(out, locations, pkg, en, [...path, 4, i], `${localName}.${en.name}`);
  });
}

function collectEnumComments(
  out: Map<string, string>,
  locations: Map<string, string>,
  pkg: string,
  en: EnumDescriptorProto,
  path: number[],
  localName: string,
): void {
  const full = joinName(pkg, localName);
  setComment(out, full, locations, path);
  en.value.forEach((value, i) => {
    setComment(out, `${full}.${value.name}`, locations, [...path, 2, i]);
  });
}

function setComment(out: Map<string, string>, key: string, locations: Map<string, string>, path: number[]): void {
  const comment = locations.get(path.join("."));
  if (comment) {
    out.set(key, comment);
  }
}

function cleanComment(comment: string): string {
  return comment
    .split(/\r?\n/)
    .map((line) => line.replace(/^\s*\* ?/, "").trim())
    .join("\n")
    .trim();
}

function joinName(pkg: string, name: string): string {
  return pkg ? `${pkg}.${name}` : name;
}

function stripLeadingDot(name: string): string {
  return name.startsWith(".") ? name.slice(1) : name;
}
