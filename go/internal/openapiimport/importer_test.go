package openapiimport

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertProducesCanonicalProto(t *testing.T) {
	fixture := filepath.Join("..", "..", "..", "testdata", "openapi")
	spec, err := os.ReadFile(filepath.Join(fixture, "library.yaml"))
	require.NoError(t, err)
	want, err := os.ReadFile(filepath.Join(fixture, "gen", "library", "v1", "library.proto"))
	require.NoError(t, err)

	result, err := Convert(spec, Options{
		Package:   "library.v1",
		GoPackage: "example.com/project/gen/library/v1",
	})
	require.NoError(t, err)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "responses/201 success status is not encoded")
	assert.Equal(t, string(want), string(result.Source))

	source := string(result.Source)
	assert.Contains(
		t,
		source,
		`option go_package = "example.com/project/gen/library/v1";`,
	)
	assert.Contains(t, source, "service LibraryService")
	assert.Contains(t, source, "rpc CreateBook(CreateBookRequest) returns (CreateBookResponse)")
	assert.Contains(t, source, `get: "/v1/books/{book_id}"`)
	assert.Contains(t, source, `body: "body"`)
	assert.Contains(t, source, `response_body: "book"`)
	assert.Contains(t, source, `json_name = "book-id"`)
	assert.Contains(t, source, "google.protobuf.Timestamp created_at = 2")
	assert.Contains(t, source, "(buf.validate.field).string.uuid = true")
	assert.Contains(t, source, `(buf.validate.field).string.in = "checked-out"`)
	assert.Contains(t, source, "map<string, string> attributes = 1")
}

func TestConvertIsIndependentOfDocumentMapOrderAndEncoding(t *testing.T) {
	yamlDocument := []byte(`openapi: 3.0.3
info:
  title: Things
  version: 1.0.0
paths:
  /things/{thing-id}:
    get:
      operationId: readThing
      parameters:
        - name: thing-id
          in: path
          required: true
          schema:
            type: string
      responses:
        "200":
          description: A thing.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Thing"
components:
  schemas:
    Thing:
      type: object
      additionalProperties: false
      properties:
        zebra:
          type: boolean
        alpha:
          type: string
`)
	jsonDocument := []byte(`{
  "components": {
    "schemas": {
      "Thing": {
        "properties": {
          "alpha": {"type": "string"},
          "zebra": {"type": "boolean"}
        },
        "additionalProperties": false,
        "type": "object"
      }
    }
  },
  "paths": {
    "/things/{thing-id}": {
      "get": {
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": {"$ref": "#/components/schemas/Thing"}
              }
            },
            "description": "A thing."
          }
        },
        "parameters": [
          {
            "schema": {"type": "string"},
            "required": true,
            "in": "path",
            "name": "thing-id"
          }
        ],
        "operationId": "readThing"
      }
    }
  },
  "info": {"version": "1.0.0", "title": "Things"},
  "openapi": "3.0.3"
}`)

	yamlResult, err := Convert(yamlDocument, Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.NoError(t, err)
	jsonResult, err := Convert(jsonDocument, Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.NoError(t, err)
	assert.Equal(t, string(yamlResult.Source), string(jsonResult.Source))
	assert.Contains(t, string(yamlResult.Source), "optional string alpha = 1")
	assert.Contains(t, string(yamlResult.Source), "optional bool zebra = 2")
}

func TestConvertResolvesOnlyBundledComponentReferences(t *testing.T) {
	recursive := []byte(`openapi: 3.1.0
info:
  title: Nodes
  version: 1.0.0
paths:
  /node:
    get:
      operationId: getNode
      responses:
        "200":
          description: A node.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Node"
components:
  schemas:
    Node:
      type: object
      additionalProperties: false
      properties:
        child:
          $ref: "#/components/schemas/Node"
        name:
          type: string
`)
	result, err := Convert(recursive, Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.NoError(t, err)
	assert.Contains(t, string(result.Source), "Node child = 1")
	assert.Equal(t, 1, strings.Count(string(result.Source), "message Node {"))

	referencedObjects := []byte(`openapi: 3.1.0
info:
  title: Referenced Objects
  version: 1.0.0
paths:
  /things/{id}:
    parameters:
      - $ref: "#/components/parameters/ThingID"
    post:
      operationId: updateThing
      requestBody:
        $ref: "#/components/requestBodies/ThingBody"
      responses:
        "200":
          $ref: "#/components/responses/ThingResponse"
components:
  parameters:
    ThingID:
      name: id
      in: path
      required: true
      schema:
        type: string
  requestBodies:
    ThingBody:
      required: true
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/ThingInput"
  responses:
    ThingResponse:
      description: Updated thing.
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Thing"
  schemas:
    Thing:
      type: object
      additionalProperties: false
      properties:
        id:
          type: string
    ThingInput:
      type: object
      additionalProperties: false
      properties:
        name:
          type: string
`)
	result, err = Convert(referencedObjects, Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.NoError(t, err)
	assert.Contains(t, string(result.Source), `post: "/things/{id}"`)
	assert.Contains(t, string(result.Source), "ThingInput body = 2")
	assert.Contains(t, string(result.Source), "Thing thing = 1")

	for _, reference := range []string{
		"https://example.invalid/schemas.yaml#/Node",
		"file:///tmp/schemas.yaml#/Node",
		"other.yaml#/Node",
	} {
		t.Run(reference, func(t *testing.T) {
			external := strings.ReplaceAll(
				string(recursive),
				"#/components/schemas/Node",
				reference,
			)
			_, err := Convert([]byte(external), Options{
				Package:   "example.v1",
				GoPackage: "example.com/project/gen/example/v1",
			})
			require.ErrorContains(t, err, "outside the input document")
		})
	}

	unresolved := strings.Replace(
		string(recursive),
		"#/components/schemas/Node",
		"#/components/schemas/Missing",
		1,
	)
	_, err = Convert([]byte(unresolved), Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.Error(t, err)
}

func TestConvertReportsExplicitPolicyWarnings(t *testing.T) {
	spec := []byte(`openapi: 3.1.0
info:
  title: Things
  version: 1.0.0
servers:
  - url: https://api.example.com
security:
  - apiKey: []
paths:
  /things:
    servers:
      - url: https://regional.example.com
    get:
      operationId: getThing
      servers:
        - url: https://operation.example.com
      security:
        - apiKey: []
      responses:
        "200":
          description: A thing.
          headers:
            X-Request-ID:
              schema:
                type: string
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Thing"
        "400":
          description: Invalid input.
          content:
            application/json:
              schema:
                type: object
                additionalProperties: false
components:
  securitySchemes:
    apiKey:
      type: apiKey
      in: header
      name: X-API-Key
  schemas:
    Thing:
      type: object
      additionalProperties: false
      properties:
        id:
          type: string
`)
	result, err := Convert(spec, Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.NoError(t, err)
	require.Len(t, result.Warnings, 7)
	assert.Contains(t, strings.Join(result.Warnings, "\n"), "caller-owned HTTP authentication policy")
	assert.Contains(t, strings.Join(result.Warnings, "\n"), "model HTTP errors as gRPC statuses")
	assert.Contains(t, strings.Join(result.Warnings, "\n"), "use gRPC response metadata explicitly")
	assert.Contains(t, strings.Join(result.Warnings, "\n"), "service endpoints remain deployment configuration")
	assert.True(t, slices.IsSorted(result.Warnings))
}

func TestConvertMapsSupportedScalarConstraintsAndEmptyRPCs(t *testing.T) {
	spec := []byte(`openapi: 3.1.0
info:
  title: Metrics
  version: 1.0.0
paths:
  /metrics:
    post:
      requestBody:
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              properties:
                blob:
                  type: string
                  format: byte
                count:
                  type: integer
                  format: int32
                  minimum: 0
                  exclusiveMinimum: 1
                  maximum: 100
                  exclusiveMaximum: 99
                email:
                  type: string
                  format: email
                enabled:
                  type: boolean
                ratio:
                  type: number
                  format: float
                  minimum: 0
                  maximum: 1
                reference:
                  type: string
                  format: uri-reference
                score:
                  type: number
                  format: double
                  exclusiveMinimum: 0
      responses:
        "200":
          description: Computed score.
          content:
            application/json:
              schema:
                type: number
                format: double
                minimum: 0
  /ping:
    get:
      responses:
        "204":
          description: Alive.
`)
	result, err := Convert(spec, Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.NoError(t, err)
	source := string(result.Source)
	assert.Contains(t, source, "rpc PostMetrics(PostMetricsRequest) returns (PostMetricsResponse)")
	assert.Contains(t, source, "optional bytes blob")
	assert.Contains(t, source, "(buf.validate.field).int32.gt = 1")
	assert.Contains(t, source, "(buf.validate.field).int32.lt = 99")
	assert.Contains(t, source, "(buf.validate.field).string.email = true")
	assert.Contains(t, source, "(buf.validate.field).string.uri_ref = true")
	assert.Contains(t, source, "(buf.validate.field).float.gte = 0")
	assert.Contains(t, source, "(buf.validate.field).float.finite = true")
	assert.Contains(t, source, "(buf.validate.field).double.gt = 0")
	assert.Contains(t, source, "(buf.validate.field).double.finite = true")
	assert.Contains(t, source, "optional double value = 1")
	assert.Contains(t, source, `option (google.api.http) = {get: "/ping"};`)
	assert.Contains(
		t,
		source,
		"rpc GetPing(google.protobuf.Empty) returns (google.protobuf.Empty)",
	)
	assert.Contains(t, source, `import "google/protobuf/empty.proto";`)
}

func TestConvertRejectsAmbiguousOrLossyContracts(t *testing.T) {
	tests := []struct {
		name      string
		spec      string
		wantError string
		options   Options
	}{
		{
			name:      "unsupported version",
			spec:      strings.Replace(validDocument, "3.1.0", "3.2.0", 1),
			wantError: "unsupported OpenAPI version",
		},
		{
			name: "custom JSON Schema dialect",
			spec: strings.Replace(
				validDocument,
				"openapi: 3.1.0",
				"openapi: 3.1.0\njsonSchemaDialect: https://example.com/custom",
				1,
			),
			wantError: "custom JSON Schema dialect",
		},
		{
			name:      "missing title",
			spec:      strings.Replace(validDocument, "title: Things", `title: ""`, 1),
			wantError: "#/info/title",
		},
		{
			name: "webhook",
			spec: validDocument + `
webhooks:
  event: {}
`,
			wantError: "#/webhooks",
		},
		{
			name:      "unsupported method",
			spec:      strings.Replace(validDocument, "    get:", "    head:", 1),
			wantError: "HTTP HEAD",
		},
		{
			name:      "header parameter",
			spec:      strings.Replace(validDocument, "in: path", "in: header", 1),
			wantError: "header parameters",
		},
		{
			name:      "optional path parameter",
			spec:      strings.Replace(validDocument, "required: true", "required: false", 1),
			wantError: "path parameters must be required",
		},
		{
			name:      "unmatched path parameter",
			spec:      strings.Replace(validDocument, "name: id", "name: other", 1),
			wantError: "has no matching path parameter",
		},
		{
			name:      "partial segment variable",
			spec:      strings.Replace(validDocument, "/things/{id}", "/things/prefix-{id}", 1),
			wantError: "must occupy a complete segment",
		},
		{
			name:      "literal path wildcard",
			spec:      strings.Replace(validDocument, "/things/{id}", "/things/*/{id}", 1),
			wantError: "wildcard",
		},
		{
			name: "date-time path parameter",
			spec: strings.Replace(
				validDocument,
				`          schema:
            type: string`,
				`          schema:
            type: string
            format: date-time`,
				1,
			),
			wantError: "path parameters must be scalar",
		},
		{
			name: "date-time array query parameter",
			spec: strings.NewReplacer(
				"/things/{id}", "/things",
				`          in: path
          required: true
          schema:
            type: string`,
				`          in: query
          schema:
            type: array
            items:
              type: string
              format: date-time`,
			).Replace(validDocument),
			wantError: "query parameters must be scalar or arrays of scalars",
		},
		{
			name: "deep object query",
			spec: strings.NewReplacer(
				"/things/{id}", "/things",
				"in: path", "in: query\n          style: deepObject",
			).Replace(validDocument),
			wantError: "query style",
		},
		{
			name: "GET request body",
			spec: strings.Replace(
				validDocument,
				"      responses:",
				`      requestBody:
        content:
          application/json:
            schema:
              type: string
      responses:`,
				1,
			),
			wantError: "request bodies are not imported",
		},
		{
			name: "multiple success responses",
			spec: strings.Replace(
				validDocument,
				`        "200":`,
				`        "201":
          description: Also successful.
        "200":`,
				1,
			),
			wantError: "exactly one explicit 2xx response",
		},
		{
			name:      "wildcard success response",
			spec:      strings.Replace(validDocument, `"200"`, `"2XX"`, 1),
			wantError: "wildcard success responses are ambiguous",
		},
		{
			name:      "non JSON response",
			spec:      strings.Replace(validDocument, "application/json", "text/plain", 1),
			wantError: "exactly application/json is supported",
		},
		{
			name: "composition",
			spec: strings.Replace(
				validDocument,
				`      type: object
      additionalProperties: false`,
				`      oneOf:
        - type: string
        - type: boolean`,
				1,
			),
			wantError: "/oneOf",
		},
		{
			name: "nullable union",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				`            type:
              - string
              - "null"`,
				1,
			),
			wantError: "nullable type unions",
		},
		{
			name: "int64",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				`            type: integer
            format: int64`,
				1,
			),
			wantError: `integers require format "int32"`,
		},
		{
			name: "numeric enum",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				`            type: integer
            format: int32
            enum: [1, 2]`,
				1,
			),
			wantError: "only string enums",
		},
		{
			name: "unformatted number",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				"            type: number",
				1,
			),
			wantError: `numbers require format "float" or "double"`,
		},
		{
			name: "unsupported string format",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				`            type: string
            format: date`,
				1,
			),
			wantError: "no canonical protobuf representation",
		},
		{
			name: "invalid string bounds",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				`            type: string
            minLength: 5
            maxLength: 2`,
				1,
			),
			wantError: "minLength exceeds maxLength",
		},
		{
			name: "default",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				`            type: string
            default: unknown`,
				1,
			),
			wantError: "/default",
		},
		{
			name: "read only",
			spec: strings.Replace(
				validDocument,
				"            type: string",
				`            type: string
            readOnly: true`,
				1,
			),
			wantError: "/readOnly",
		},
		{
			name:      "open object",
			spec:      strings.Replace(validDocument, "      additionalProperties: false\n", "", 1),
			wantError: "/additionalProperties",
		},
		{
			name: "properties mixed with map values",
			spec: strings.Replace(
				validDocument,
				"      additionalProperties: false",
				`      additionalProperties:
        type: string`,
				1,
			),
			wantError: "cannot mix named properties",
		},
		{
			name: "required repeated field",
			spec: strings.Replace(
				validDocument,
				`      additionalProperties: false
      properties:
        value:
          type: string`,
				`      additionalProperties: false
      required:
        - value
      properties:
        value:
          type: array
          items:
            type: string`,
				1,
			),
			wantError: "required arrays and maps",
		},
		{
			name: "positive array lower bound",
			spec: strings.Replace(
				validDocument,
				`        value:
          type: string`,
				`        value:
          type: array
          minItems: 1
          items:
            type: string`,
				1,
			),
			wantError: "cannot preserve absent versus empty collection",
		},
		{
			name: "positive map lower bound",
			spec: strings.Replace(
				validDocument,
				`        value:
          type: string`,
				`        value:
          type: object
          minProperties: 1
          additionalProperties:
            type: string`,
				1,
			),
			wantError: "cannot preserve absent versus empty collection",
		},
		{
			name: "unique repeated messages",
			spec: strings.Replace(
				validDocument,
				`        value:
          type: string`,
				`        value:
          type: array
          uniqueItems: true
          items:
            type: object
            additionalProperties: false
            properties:
              id:
                type: string`,
				1,
			),
			wantError: "uniqueness is not defined for repeated message",
		},
		{
			name: "normalized field collision",
			spec: strings.Replace(
				validDocument,
				`        value:
          type: string`,
				`        thing-id:
          type: string
        thingID:
          type: string`,
				1,
			),
			wantError: "collides",
		},
		{
			name: "RPC collision",
			spec: strings.Replace(
				validDocument,
				"components:",
				`  /other:
    get:
      operationId: getThing
      responses:
        "204":
          description: Empty.
components:`,
				1,
			),
			wantError: "RPC name",
		},
		{
			name:      "invalid package",
			spec:      validDocument,
			wantError: "lowercase protobuf package",
			options: Options{
				Package:   "Example.V1",
				GoPackage: "example.com/project/gen/example/v1",
			},
		},
		{
			name:      "reserved package segment",
			spec:      validDocument,
			wantError: "reserved protobuf keyword",
			options: Options{
				Package:   "example.package.v1",
				GoPackage: "example.com/project/gen/example/v1",
			},
		},
		{
			name:      "invalid Go package",
			spec:      validDocument,
			wantError: "portable Go import path",
			options: Options{
				Package:   "example.v1",
				GoPackage: "local",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := test.options
			if options.Package == "" {
				options.Package = "example.v1"
			}
			if options.GoPackage == "" {
				options.GoPackage = "example.com/project/gen/example/v1"
			}
			_, err := Convert([]byte(test.spec), options)
			require.ErrorContains(t, err, test.wantError)
		})
	}

	_, err := Convert(nil, Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.EqualError(t, err, "OpenAPI document is empty")
	_, err = Convert(make([]byte, MaxDocumentBytes+1), Options{
		Package:   "example.v1",
		GoPackage: "example.com/project/gen/example/v1",
	})
	require.ErrorContains(t, err, "maximum")
}

func TestProtobufFieldNumbersSkipTheReservedRange(t *testing.T) {
	assert.Equal(t, 18999, protobufFieldNumber(18998))
	assert.Equal(t, 20000, protobufFieldNumber(18999))
	assert.Equal(t, 20001, protobufFieldNumber(19000))
}

const validDocument = `openapi: 3.1.0
info:
  title: Things
  version: 1.0.0
paths:
  /things/{id}:
    get:
      operationId: getThing
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
      responses:
        "200":
          description: A thing.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Thing"
components:
  schemas:
    Thing:
      type: object
      additionalProperties: false
      properties:
        value:
          type: string
`
