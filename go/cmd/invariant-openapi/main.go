// invariant-openapi performs a one-way import of an OpenAPI document into a
// reviewable protobuf contract.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jim-technologies/invariantprotocol/go/internal/openapiimport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "invariant-openapi: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("command required: import")
	}

	switch args[0] {
	case "import":
		return runImport(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		writeUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q; expected import", args[0])
	}
}

func writeUsage(output io.Writer) {
	fmt.Fprintln(output, "usage:")
	fmt.Fprintln(output, "  invariant-openapi import --input FILE --package PACKAGE --go-package IMPORT [--service NAME] --output FILE|-")
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "The generated .proto is a one-way bootstrap artifact. Review it, then maintain protobuf as the canonical contract.")
}

func runImport(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var inputPath, packageName, goPackage, serviceName, outputPath string
	flags.StringVar(&inputPath, "input", "", "bundled OpenAPI 3.0 or 3.1 input")
	flags.StringVar(&packageName, "package", "", "lowercase protobuf package")
	flags.StringVar(&goPackage, "go-package", "", "generated Go import path")
	flags.StringVar(&serviceName, "service", "", "protobuf service name (defaults from info.title)")
	flags.StringVar(&outputPath, "output", "", "protobuf output file, or - for stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf(
			"import: unexpected positional arguments: %s",
			strings.Join(flags.Args(), " "),
		)
	}
	if inputPath == "" {
		return errors.New("import: --input is required")
	}
	if packageName == "" {
		return errors.New("import: --package is required")
	}
	if goPackage == "" {
		return errors.New("import: --go-package is required")
	}
	if outputPath == "" {
		return errors.New("import: --output is required")
	}

	spec, err := readBounded(inputPath)
	if err != nil {
		return fmt.Errorf("import: read input %q: %w", inputPath, err)
	}
	result, err := openapiimport.Convert(spec, openapiimport.Options{
		Package:   packageName,
		GoPackage: goPackage,
		Service:   serviceName,
	})
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(stderr, "warning: %s\n", warning); err != nil {
			return fmt.Errorf("import: write warning: %w", err)
		}
	}
	if outputPath == "-" {
		if _, err := stdout.Write(result.Source); err != nil {
			return fmt.Errorf("import: write stdout: %w", err)
		}
		return nil
	}
	if err := writeFileAtomically(outputPath, result.Source); err != nil {
		return fmt.Errorf("import: write output %q: %w", outputPath, err)
	}
	return nil
}

func readBounded(path string) ([]byte, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()

	spec, err := io.ReadAll(io.LimitReader(input, openapiimport.MaxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(spec) > openapiimport.MaxDocumentBytes {
		return nil, fmt.Errorf(
			"document exceeds the %d-byte limit",
			openapiimport.MaxDocumentBytes,
		)
	}
	return spec, nil
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
