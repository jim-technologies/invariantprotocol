package postgres

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jim-technologies/invariantprotocol/go/data"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
)

func TestDDLPreservesProtobufPresenceAndAddsOneofConstraint(t *testing.T) {
	dataset, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	dataset.Description = "owner's canonical record"
	datasetField(t, dataset, "string_value").Description = "owner's label"

	ddl, diagnostics, err := DDL(dataset)
	require.NoError(t, err)
	require.Contains(t, ddl, fmt.Sprintf("CREATE TABLE %q (", dataset.GetName()))
	require.Contains(t, ddl, `"double_value" DOUBLE PRECISION NOT NULL DEFAULT 0`)
	require.Contains(t, ddl, `"uint64_value" NUMERIC(20,0) NOT NULL DEFAULT 0`)
	require.Contains(t, ddl, `"uint32_value" BIGINT NOT NULL DEFAULT 0`)
	require.Contains(t, ddl, `"bool_value" BOOLEAN NOT NULL DEFAULT FALSE`)
	require.Contains(t, ddl, `"string_value" TEXT NOT NULL DEFAULT ''`)
	require.Contains(t, ddl, `"bytes_value" BYTEA NOT NULL DEFAULT '\x'::bytea`)
	require.Contains(t, ddl, `"state" INTEGER NOT NULL DEFAULT 0`)
	require.Contains(t, ddl, `"optional_note" TEXT`)
	require.NotContains(t, ddl, `"optional_note" TEXT NOT NULL`)
	require.Contains(t, ddl, `"nested" JSONB`)
	require.Contains(t, ddl, `"labels" JSONB NOT NULL DEFAULT '[]'::jsonb`)
	require.Contains(t, ddl, `"counters" JSONB NOT NULL DEFAULT '{}'::jsonb`)
	require.Contains(t, ddl, `"created_at" TIMESTAMPTZ`)
	require.Contains(t, ddl, `"elapsed" BIGINT`)
	require.Contains(t, ddl, `CHECK (num_nonnulls("choice_count", "choice_name") <= 1)`)
	require.Contains(t, ddl, `IS 'owner''s canonical record';`)
	require.Contains(t, ddl, `IS 'owner''s label';`)
	require.True(t, strings.HasSuffix(ddl, ";\n"))

	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_PRECISION_REDUCED, diagnostic(t, diagnostics, "created_at").GetCompatibility())
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, diagnostic(t, diagnostics, "elapsed").GetCompatibility())
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, diagnostics, "state").GetCompatibility())
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, diagnostics, "choice_count").GetCompatibility())
	for _, path := range []string{"string_value", "nested", "nested.label", "labels", "labels[]", "counters", "counters.key", "attributes", "opaque"} {
		nulDiagnostic := diagnostic(t, diagnostics, path)
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, nulDiagnostic.GetCompatibility(), path)
		require.Contains(t, nulDiagnostic.GetMessage(), "U+0000", path)
	}
	require.Contains(t, diagnostic(t, diagnostics, "attributes").GetMessage(), "numbers to be finite")
	require.Contains(t, diagnostic(t, diagnostics, "opaque").GetMessage(), "type URL to resolve")

	datasetField(t, dataset, "state").GetType().GetEnum().Closed = true
	_, closedDiagnostics, err := DDL(dataset)
	require.NoError(t, err)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, diagnostic(t, closedDiagnostics, "state").GetCompatibility())
	require.Contains(t, diagnostic(t, closedDiagnostics, "state").GetMessage(), "admits undeclared values")

	proto2, err := data.CompileMessage((&greetpb.Proto2Record{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	proto2DDL, _, err := DDL(proto2)
	require.NoError(t, err)
	require.Contains(t, proto2DDL, `"id" BIGINT NOT NULL`)
	require.NotContains(t, proto2DDL, `"id" BIGINT NOT NULL DEFAULT`)
	require.Contains(t, proto2DDL, `"label" TEXT`)
	require.NotContains(t, proto2DDL, `"label" TEXT DEFAULT 'unknown'`)
}

func TestIdentifierIsDeterministicAndBounded(t *testing.T) {
	long := strings.Repeat("schema_", 20) + `"quoted`
	first, err := identifier(long)
	require.NoError(t, err)
	second, err := identifier(long)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.LessOrEqual(t, len(first), maxIdentifierBytes)
	require.Equal(t, `"a""b"`, quoteIdentifier(`a"b`))
}

func datasetField(t *testing.T, dataset *datav1.DatasetSchema, name string) *datav1.Field {
	t.Helper()
	for _, field := range dataset.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	require.FailNow(t, "missing dataset field", "name %q", name)
	return nil
}

func diagnostic(t *testing.T, diagnostics []*datav1.MappingDiagnostic, path string) *datav1.MappingDiagnostic {
	t.Helper()
	for _, item := range diagnostics {
		if item.GetFieldPath() == path {
			return item
		}
	}
	require.FailNow(t, "missing diagnostic", "path %q", path)
	return nil
}
