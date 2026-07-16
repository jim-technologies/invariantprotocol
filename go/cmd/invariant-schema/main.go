// invariant-schema compiles protobuf descriptors into Invariant's canonical
// data-schema bundle and renders that bundle for storage systems.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jim-technologies/invariantprotocol/go/data"
	invariantarrow "github.com/jim-technologies/invariantprotocol/go/data/arrow"
	"github.com/jim-technologies/invariantprotocol/go/data/iceberg"
	"github.com/jim-technologies/invariantprotocol/go/data/parquet"
	"github.com/jim-technologies/invariantprotocol/go/data/postgres"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"google.golang.org/protobuf/proto"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "invariant-schema: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("command required: compile, arrow, parquet, iceberg, or sql")
	}

	switch args[0] {
	case "compile":
		return runCompile(args[1:], stderr)
	case "arrow", "parquet", "iceberg", "sql":
		return runRender(args[0], args[1:], stdout, stderr)
	case "help", "-h", "--help":
		writeUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q; expected compile, arrow, parquet, iceberg, or sql", args[0])
	}
}

func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  invariant-schema compile --descriptor FILE --message FULL_NAME [--message FULL_NAME ...] --output FILE")
	fmt.Fprintln(w, "  invariant-schema arrow|parquet|iceberg|sql --bundle FILE [--message FULL_NAME] [--output FILE|-]")
}

type messageFlags []string

func (f *messageFlags) String() string { return strings.Join(*f, ",") }

func (f *messageFlags) Set(value string) error {
	if value == "" {
		return errors.New("message name must not be empty")
	}
	*f = append(*f, value)
	return nil
}

func runCompile(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("compile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var descriptorPath, outputPath string
	var messages messageFlags
	flags.StringVar(&descriptorPath, "descriptor", "", "FileDescriptorSet input")
	flags.Var(&messages, "message", "fully-qualified root message name (repeatable)")
	flags.StringVar(&outputPath, "output", "", "SchemaBundle output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("compile: unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if descriptorPath == "" {
		return errors.New("compile: --descriptor is required")
	}
	if len(messages) == 0 {
		return errors.New("compile: at least one --message is required")
	}
	if outputPath == "" {
		return errors.New("compile: --output is required")
	}
	if outputPath == "-" {
		return errors.New("compile: --output must be a file so schema identity history can be retained")
	}

	slices.Sort(messages)
	for index := 1; index < len(messages); index++ {
		if messages[index] == messages[index-1] {
			return fmt.Errorf("compile: duplicate --message %q", messages[index])
		}
	}

	descriptor, err := os.ReadFile(descriptorPath)
	if err != nil {
		return fmt.Errorf("compile: read descriptor %q: %w", descriptorPath, err)
	}

	previous, err := loadPreviousBundle(outputPath)
	if err != nil {
		return err
	}
	bundle, err := data.CompileDescriptorBytes(descriptor, messages, previous)
	if err != nil {
		return fmt.Errorf("compile descriptor: %w", err)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("compile: marshal schema bundle: %w", err)
	}
	if err := writeFileAtomically(outputPath, encoded); err != nil {
		return fmt.Errorf("compile: write schema bundle %q: %w", outputPath, err)
	}
	return nil
}

func loadPreviousBundle(path string) (*datav1.SchemaBundle, error) {
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compile: read previous schema bundle %q: %w", path, err)
	}
	previous := new(datav1.SchemaBundle)
	if err := proto.Unmarshal(encoded, previous); err != nil {
		return nil, fmt.Errorf("compile: decode previous schema bundle %q: %w", path, err)
	}
	return previous, nil
}

func runRender(target string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet(target, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var bundlePath, messageName, outputPath string
	flags.StringVar(&bundlePath, "bundle", "", "SchemaBundle input")
	flags.StringVar(&messageName, "message", "", "fully-qualified source message name")
	flags.StringVar(&outputPath, "output", "-", "artifact output file, or - for stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s: unexpected positional arguments: %s", target, strings.Join(flags.Args(), " "))
	}
	if bundlePath == "" {
		return fmt.Errorf("%s: --bundle is required", target)
	}
	if outputPath == "" {
		return fmt.Errorf("%s: --output must not be empty", target)
	}

	bundle, err := readBundle(bundlePath)
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	if err := validateBundleVersion(bundle); err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	dataset, err := selectDataset(bundle, messageName)
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}

	artifact, diagnostics, err := render(target, dataset)
	writeDiagnostics(stderr, target, diagnostics)
	if err != nil {
		return err
	}
	if outputPath == "-" {
		if _, err := stdout.Write(artifact); err != nil {
			return fmt.Errorf("%s: write stdout: %w", target, err)
		}
		return nil
	}
	if err := writeFileAtomically(outputPath, artifact); err != nil {
		return fmt.Errorf("%s: write artifact %q: %w", target, outputPath, err)
	}
	return nil
}

func readBundle(path string) (*datav1.SchemaBundle, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema bundle %q: %w", path, err)
	}
	bundle := new(datav1.SchemaBundle)
	if err := proto.Unmarshal(encoded, bundle); err != nil {
		return nil, fmt.Errorf("decode schema bundle %q: %w", path, err)
	}
	return bundle, nil
}

func validateBundleVersion(bundle *datav1.SchemaBundle) error {
	if bundle.GetIrVersion() != data.IRVersion {
		return fmt.Errorf("bundle ir_version is %d, want %d", bundle.GetIrVersion(), data.IRVersion)
	}
	if bundle.GetMappingVersion() != data.MappingVersion {
		return fmt.Errorf("bundle mapping_version is %d, want %d", bundle.GetMappingVersion(), data.MappingVersion)
	}
	return nil
}

func selectDataset(bundle *datav1.SchemaBundle, messageName string) (*datav1.DatasetSchema, error) {
	if messageName == "" {
		switch len(bundle.GetDatasets()) {
		case 0:
			return nil, errors.New("bundle contains no datasets")
		case 1:
			return bundle.GetDatasets()[0], nil
		default:
			return nil, fmt.Errorf("--message is required because bundle contains %d datasets", len(bundle.GetDatasets()))
		}
	}

	var selected *datav1.DatasetSchema
	for _, dataset := range bundle.GetDatasets() {
		if dataset != nil && dataset.GetSourceMessage() == messageName {
			if selected != nil {
				return nil, fmt.Errorf("bundle contains duplicate dataset %q", messageName)
			}
			selected = dataset
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("bundle does not contain message %q", messageName)
	}
	return selected, nil
}

func render(target string, dataset *datav1.DatasetSchema) ([]byte, []*datav1.MappingDiagnostic, error) {
	switch target {
	case "arrow":
		schema, diagnostics, err := invariantarrow.Schema(dataset)
		if err != nil {
			return nil, diagnostics, fmt.Errorf("arrow: render message %q: %w", dataset.GetSourceMessage(), err)
		}
		var output bytes.Buffer
		if err := invariantarrow.WriteIPC(&output, schema); err != nil {
			return nil, diagnostics, fmt.Errorf("arrow: write IPC stream for message %q: %w", dataset.GetSourceMessage(), err)
		}
		return output.Bytes(), diagnostics, nil
	case "parquet":
		schema, diagnostics, err := parquet.Schema(dataset)
		if err != nil {
			return nil, diagnostics, fmt.Errorf("parquet: render message %q: %w", dataset.GetSourceMessage(), err)
		}
		return withFinalNewline([]byte(schema.String())), diagnostics, nil
	case "iceberg":
		schema, diagnostics, err := iceberg.Schema(dataset)
		if err != nil {
			return nil, diagnostics, fmt.Errorf("iceberg: render message %q: %w", dataset.GetSourceMessage(), err)
		}
		encoded, err := iceberg.JSON(schema)
		if err != nil {
			return nil, diagnostics, fmt.Errorf("iceberg: encode message %q: %w", dataset.GetSourceMessage(), err)
		}
		return withFinalNewline(encoded), diagnostics, nil
	case "sql":
		ddl, diagnostics, err := postgres.DDL(dataset)
		if err != nil {
			return nil, diagnostics, fmt.Errorf("sql: render message %q: %w", dataset.GetSourceMessage(), err)
		}
		return withFinalNewline([]byte(ddl)), diagnostics, nil
	default:
		return nil, nil, fmt.Errorf("unsupported render target %q", target)
	}
}

func writeDiagnostics(w io.Writer, target string, diagnostics []*datav1.MappingDiagnostic) {
	for _, diagnostic := range diagnostics {
		if diagnostic == nil {
			continue
		}
		fmt.Fprintf(w, "%s: %s: %s: %s\n", target, diagnostic.GetCompatibility(), diagnostic.GetFieldPath(), diagnostic.GetMessage())
	}
}

func withFinalNewline(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return append(data, '\n')
	}
	return data
}

func writeFileAtomically(path string, data []byte) error {
	directory := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
