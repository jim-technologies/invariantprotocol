package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jim-technologies/invariantprotocol/go/internal/openapiimport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportWritesStdoutAndAtomicallyReplacesFiles(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "openapi.yaml")
	require.NoError(t, os.WriteFile(input, []byte(cliDocument), 0o600))

	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{
		"import",
		"--input", input,
		"--package", "example.v1",
		"--go-package", "example.com/project/gen/example/v1",
		"--service", "Catalog",
		"--output", "-",
	}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "service CatalogService")
	assert.Contains(t, stdout.String(), "rpc GetThing(GetThingRequest) returns (GetThingResponse)")
	assert.Empty(t, stderr.String())

	output := filepath.Join(directory, "catalog.proto")
	require.NoError(t, os.WriteFile(output, []byte("previous"), 0o600))
	stdout.Reset()
	require.NoError(t, run([]string{
		"import",
		"--input", input,
		"--package", "example.v1",
		"--go-package", "example.com/project/gen/example/v1",
		"--output", output,
	}, &stdout, &stderr))
	assert.Empty(t, stdout.String())
	generated, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Contains(t, string(generated), "service ThingsService")
	info, err := os.Stat(output)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	matches, err := filepath.Glob(filepath.Join(directory, ".catalog.proto.*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestImportLeavesExistingOutputUntouchedOnFailure(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "openapi.yaml")
	output := filepath.Join(directory, "contract.proto")
	require.NoError(t, os.WriteFile(input, []byte("openapi: 3.2.0\n"), 0o600))
	require.NoError(t, os.WriteFile(output, []byte("keep me"), 0o600))

	var stdout, stderr bytes.Buffer
	err := run([]string{
		"import",
		"--input", input,
		"--package", "example.v1",
		"--go-package", "example.com/project/gen/example/v1",
		"--output", output,
	}, &stdout, &stderr)
	require.Error(t, err)
	assert.Empty(t, stdout.String())
	contents, readErr := os.ReadFile(output)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(contents))
}

func TestImportReportsWarningsSeparatelyFromGeneratedSource(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "openapi.yaml")
	warningDocument := strings.Replace(
		cliDocument,
		"paths:",
		"security:\n  - bearer: []\npaths:",
		1,
	)
	require.NoError(t, os.WriteFile(input, []byte(warningDocument), 0o600))

	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{
		"import",
		"--input", input,
		"--package", "example.v1",
		"--go-package", "example.com/project/gen/example/v1",
		"--output", "-",
	}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "syntax = \"proto3\"")
	assert.NotContains(t, stdout.String(), "warning:")
	assert.Contains(t, stderr.String(), "warning: OpenAPI security requirements")

	err := run([]string{
		"import",
		"--input", input,
		"--package", "example.v1",
		"--go-package", "example.com/project/gen/example/v1",
		"--output", "-",
	}, &stdout, failingWriter{})
	require.ErrorContains(t, err, "write warning")
}

func TestImportCommandValidationAndIOFailures(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"help"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "invariant-openapi import")

	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{name: "command", wantError: "command required"},
		{name: "unknown", args: []string{"serve"}, wantError: `unknown command "serve"`},
		{name: "input", args: []string{"import"}, wantError: "--input is required"},
		{
			name:      "package",
			args:      []string{"import", "--input", "missing"},
			wantError: "--package is required",
		},
		{
			name:      "go package",
			args:      []string{"import", "--input", "missing", "--package", "example.v1"},
			wantError: "--go-package is required",
		},
		{
			name: "output",
			args: []string{
				"import", "--input", "missing", "--package", "example.v1",
				"--go-package", "example.com/project/gen/example/v1",
			},
			wantError: "--output is required",
		},
		{
			name: "positionals",
			args: []string{
				"import", "--input", "missing", "--package", "example.v1",
				"--go-package", "example.com/project/gen/example/v1", "--output", "-", "extra",
			},
			wantError: "unexpected positional arguments",
		},
		{
			name: "missing file",
			args: []string{
				"import", "--input", "missing", "--package", "example.v1",
				"--go-package", "example.com/project/gen/example/v1", "--output", "-",
			},
			wantError: "read input",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			err := run(test.args, &stdout, &stderr)
			require.ErrorContains(t, err, test.wantError)
		})
	}

	err := run([]string{"import", "-h"}, &stdout, &stderr)
	require.ErrorIs(t, err, flag.ErrHelp)
}

func TestImportEnforcesInputAndOutputBoundaries(t *testing.T) {
	directory := t.TempDir()
	oversized := filepath.Join(directory, "large.yaml")
	require.NoError(
		t,
		os.WriteFile(oversized, bytes.Repeat([]byte{'x'}, openapiimport.MaxDocumentBytes+1), 0o600),
	)
	_, err := readBounded(oversized)
	require.ErrorContains(t, err, "exceeds")

	input := filepath.Join(directory, "openapi.yaml")
	require.NoError(t, os.WriteFile(input, []byte(cliDocument), 0o600))
	var stdout, stderr bytes.Buffer
	err = run([]string{
		"import",
		"--input", input,
		"--package", "example.v1",
		"--go-package", "example.com/project/gen/example/v1",
		"--output", filepath.Join(directory, "missing", "contract.proto"),
	}, &stdout, &stderr)
	require.ErrorContains(t, err, "write output")

	err = run([]string{
		"import",
		"--input", input,
		"--package", "example.v1",
		"--go-package", "example.com/project/gen/example/v1",
		"--output", "-",
	}, failingWriter{}, &stderr)
	require.ErrorContains(t, err, "write stdout")

	err = writeFileAtomically(directory, []byte("cannot replace a directory"))
	require.Error(t, err)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("intentional write failure")
}

const cliDocument = `openapi: 3.1.0
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
        name:
          type: string
`
