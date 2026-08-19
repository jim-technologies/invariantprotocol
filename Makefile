.DEFAULT_GOAL := help

.PHONY: help build validate validate-static release version-check parity parity-release git-install-check connect-interop postgres-integration clickhouse-integration lance-integration data-integration integration lint fmt fmt-check go-mod-check test test-go race-go test-python test-rust test-typescript coverage coverage-go coverage-python coverage-rust coverage-typescript typecheck proto-comments public-surface security bench generate openapi-codegen-check deps verify-generate breaking

BASE_REF ?= origin/main

help: ## Show available make targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: node_modules/.package-lock.json ## Build every language package and command.
	GOFLAGS=-mod=readonly go build ./...
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; cd python && uv build --out-dir "$$tmp"
	cd rust && cargo build --workspace --locked
	npm run build

# The single gate (see MAKEFILE-CONTRACT.md). CI runs the same slices as
# separate jobs so failures are easy to identify and the suites run in parallel.
validate: validate-static verify-generate coverage race-go ## Run the full gate: static checks, generated-code staleness, coverage-gated tests, and the Go race detector.

validate-static: version-check parity fmt-check lint typecheck proto-comments breaking public-surface go-mod-check ## Run the static slice of validate: formatting, lint, type, schema, breaking-change, and policy checks.

node_modules/.package-lock.json: package-lock.json package.json
	npm ci --ignore-scripts

version-check: ## Verify every language package uses the root VERSION.
	python3 scripts/check_versions.py

parity: ## Validate and report the cross-language feature contract.
	python3 scripts/check_feature_parity.py

parity-release: ## Reject a release while any core feature lacks four-language support.
	python3 scripts/check_feature_parity.py --release

release: ## Verify release readiness from the root VERSION; refuses a dirty or unpushed tree.
	scripts/release.sh

git-install-check: ## Install every language package from the current Git commit.
	scripts/check_git_installs.sh

connect-interop: node_modules/.package-lock.json ## Exercise Go, Python, and Rust HTTP projections with Connect-ES.
	@set -eu; \
	tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	rust_target="$${CARGO_TARGET_DIR:-rust/target}"; \
	case "$$rust_target" in /*) ;; *) rust_target="$(CURDIR)/$$rust_target" ;; esac; \
	GOFLAGS=-mod=readonly go build -o "$$tmp/go-connect-interop" ./go/tests/connectinterop; \
	uv sync --locked --project python; \
	cargo build --manifest-path rust/Cargo.toml --locked --target-dir "$$rust_target" --example connect_interop_server; \
	INVARIANT_CONNECT_INTEROP_GO="$$tmp/go-connect-interop" \
	INVARIANT_CONNECT_INTEROP_RUST="$$rust_target/debug/examples/connect_interop_server" \
	node typescript/tests/connect_interop.ts

postgres-integration: ## Apply and round-trip generated PostgreSQL through Atlas.
	scripts/check_postgres_atlas.sh

clickhouse-integration: ## Apply generated declarations and round-trip values through ClickHouse.
	scripts/check_clickhouse.sh

lance-integration: ## Exercise invariant-generated Arrow data through a local LanceDB lifecycle.
	cd python && uv run python -m pytest tests/test_data_lance.py

data-integration: postgres-integration clickhouse-integration lance-integration ## Exercise every external data-schema boundary.

integration: git-install-check connect-interop data-integration ## Exercise Git installs and external protocol/data boundaries.

fmt-check: node_modules/.package-lock.json ## Verify formatting without modifying files.
	test -z "$$(gofmt -l go)" || { echo "gofmt: files need formatting:"; gofmt -l go; exit 1; }
	test -z "$$(gofmt -l scripts/generate_cdc_v2_fixtures.go)" || { echo "gofmt: CDC v2 fixture generator needs formatting"; exit 1; }
	cd python && ruff format --check src/ tests/ ../scripts/
	cd rust && cargo fmt --all --check
	cd proto && buf format --diff --exit-code
	buf format --config buf.data.yaml --diff --exit-code testdata/schema/test/v1/annotated.proto
	cd testdata/openapi && buf format --diff --exit-code gen/library/v1/library.proto
	cd conformance/proto && buf format --diff --exit-code
	npm run format:check

lint: node_modules/.package-lock.json ## Run Go, Python, Rust, and proto linters.
	actionlint
	shellcheck scripts/*.sh
	golangci-lint run ./...
	cd python && ruff check src/ tests/ ../scripts/
	cd rust && cargo clippy --workspace --all-targets --locked -- -D warnings
	cd proto && buf lint
	buf lint --config buf.data.yaml --path testdata/schema/test/v1/annotated.proto
	cd testdata/openapi && buf lint
	cd conformance/proto && buf lint
	npm run lint

typecheck: node_modules/.package-lock.json ## Run Python and TypeScript static type checks.
	cd python && uv run mypy
	npm run typecheck

proto-comments: ## Verify projected proto comments are complete.
	cd python && uv run invariant-check-proto-comments tests/proto/descriptor.binpb

public-surface: ## Scan OSS-facing files for private/product-specific references.
	python3 scripts/check_public_surface.py

go-mod-check: ## Verify Go dependency metadata is canonical and complete.
	go mod tidy -diff

security: node_modules/.package-lock.json ## Scan secrets and verify/audit every dependency graph.
	gitleaks git --no-banner --redact .
	go mod verify
	GOFLAGS=-mod=readonly govulncheck -test ./...
	cd python && uv lock --check && uv run pip-audit
	npm audit signatures
	npm audit --audit-level=moderate
	cd rust && cargo fetch --locked && cargo audit

fmt: node_modules/.package-lock.json ## Format code and apply safe linter fixes.
	gofmt -w go scripts/generate_cdc_v2_fixtures.go && golangci-lint run --fix ./...
	cd python && ruff format src/ tests/ ../scripts/ && ruff check --fix src/ tests/ ../scripts/
	cd rust && cargo fmt --all
	cd proto && buf format -w
	buf format --config buf.data.yaml -w testdata/schema/test/v1/annotated.proto
	cd testdata/openapi && buf format -w gen/library/v1/library.proto
	cd conformance/proto && buf format -w
	npm run format

test: test-go test-python test-rust test-typescript ## Run all language test suites.

test-go: ## Run Go unit and transport-integration tests.
	GOFLAGS=-mod=readonly go test ./...

race-go: ## Run the concurrent Go runtime under the race detector.
	GOFLAGS=-mod=readonly go test -count=1 -race ./...

test-python: ## Run Python unit and transport-integration tests.
	cd python && uv run python -m pytest tests/

test-rust: ## Run Rust unit and transport-integration tests.
	cd rust && cargo test --workspace --all-targets --locked

test-typescript: node_modules/.package-lock.json ## Run TypeScript unit and transport-integration tests.
	npm test

coverage: coverage-go coverage-python coverage-rust coverage-typescript ## Run tests with maintained coverage floors.

coverage-go: ## Run Go tests and enforce authored-code statement coverage.
	@set -eu; \
	all_packages="$$(GOFLAGS=-mod=readonly go list ./go/...)"; \
	packages="$$(printf '%s\n' "$$all_packages" | grep -Ev '/go/(gen/|tests/(connectinterop|gen|manual)$$)')"; \
	profile="$$(mktemp)"; \
	trap 'rm -f "$$profile"' EXIT; \
	GOFLAGS=-mod=readonly go test -count=1 -covermode=atomic -coverprofile="$$profile" $$packages; \
	total="$$(go tool cover -func="$$profile" | awk '/^total:/ {gsub("%", "", $$3); print $$3}')"; \
	awk -v total="$$total" 'BEGIN { printf "Go authored statement coverage: %.1f%% (required: 80.0%%)\n", total; exit !(total >= 80.0) }'

coverage-python: ## Run Python tests with branch coverage.
	cd python && uv run python -m pytest --cov=invariant --cov-branch --cov-report=term-missing tests/

coverage-rust: ## Run Rust tests with LLVM source coverage.
	cd rust && LLVM_COV="$$(command -v llvm-cov)" LLVM_PROFDATA="$$(command -v llvm-profdata)" cargo llvm-cov --workspace --locked --ignore-filename-regex '/target/' --fail-under-lines 80

coverage-typescript: node_modules/.package-lock.json ## Run TypeScript tests with V8 coverage.
	npm run test:coverage

bench: ## Run Go, Python, and Rust benchmarks.
	GOFLAGS=-mod=readonly go test -bench=. -benchtime=2s -run=^$$ ./...
	cd python && uv run python bench/bench.py
	cd rust && cargo bench --locked --bench bench -- --warm-up-time 1 --measurement-time 2

generate: node_modules/.package-lock.json ## Regenerate committed build artifacts.
	# Remove only configured generator outputs. Keep Python package markers and
	# hand-written files; this makes deleted/renamed protos delete stale bindings.
	find go/gen go/tests/gen -type f -name '*.pb.go' -delete
	find python/src/invariant/gen python/src/buf python/tests/proto/gen -type f \( -name '*_pb2.py' -o -name '*_pb2.pyi' -o -name '*_pb2_grpc.py' \) -delete
	find typescript/src/gen typescript/tests/gen -type f -name '*_pb.ts' -delete
	cd proto && buf build -o descriptor.binpb
	cd proto && NODE_NO_WARNINGS=1 buf generate descriptor.binpb
	cd proto && NODE_NO_WARNINGS=1 buf generate --template buf.googleapis.gen.yaml
	cd python/tests/proto && buf build -o descriptor.binpb
	cd python/tests/proto && buf generate descriptor.binpb
	cd python/tests/proto && buf generate --template buf.validate.gen.yaml
	cd python/tests/proto && uv run --locked --project ../.. python -m grpc_tools.protoc --descriptor_set_in=descriptor.binpb --grpc_python_out=gen greet.proto
	cd conformance/proto && buf build -o descriptor.binpb
	cd conformance/proto && buf generate descriptor.binpb
	cd conformance/proto && uv run --locked --project ../../python python -m grpc_tools.protoc --descriptor_set_in=descriptor.binpb --grpc_python_out=../../python/tests/proto/gen invariantprotocol/conformance/v1/native_cardinality.proto
	buf build --config buf.data.yaml --path testdata/schema/test/v1/annotated.proto -o testdata/schema/descriptor.binpb
	GOFLAGS=-mod=readonly go run ./go/cmd/invariant-schema compile --descriptor testdata/schema/descriptor.binpb --output testdata/schema/schema.binpb
	GOFLAGS=-mod=readonly go run ./go/cmd/invariant-schema compile --descriptor python/tests/proto/descriptor.binpb --message data.v1.CanonicalRecord --message data.v1.Proto2Record --output testdata/data.schema.binpb
	mkdir -p testdata/openapi/gen/library/v1
	GOFLAGS=-mod=readonly go run ./go/cmd/invariant-openapi import --input testdata/openapi/library.yaml --package library.v1 --go-package example.com/project/gen/library/v1 --output testdata/openapi/gen/library/v1/library.proto
	cd testdata/openapi && buf format -w gen/library/v1/library.proto
	cd testdata/openapi && buf build -o descriptor.binpb
	GOFLAGS=-mod=readonly go run ./scripts/generate_cdc_v2_fixtures.go

openapi-codegen-check: node_modules/.package-lock.json ## Compile the imported OpenAPI fixture through every language toolchain.
	scripts/check_openapi_codegen.sh

deps: ## Tidy/update language dependency lockfiles.
	flox upgrade
	go get -u all && go mod tidy
	cd python && uv lock --upgrade
	cd rust && cargo update
	npm update
	cd python/tests/proto && buf dep update
	cd testdata/openapi && buf dep update

breaking: ## Check proto breaking changes against BASE_REF.
	cd proto && buf breaking --against "../.git#ref=$(BASE_REF),subdir=proto"

verify-generate: ## Verify generated build artifacts are committed.
	$(MAKE) generate
	@if [ -n "$$(git status --porcelain --untracked-files=all -- proto/descriptor.binpb conformance/proto/descriptor.binpb go/gen go/tests/gen python/src/buf python/src/invariant/gen python/tests/proto/descriptor.binpb python/tests/proto/gen testdata/cdc/v2 testdata/data.schema.binpb testdata/openapi/descriptor.binpb testdata/openapi/gen/library/v1/library.proto testdata/schema/descriptor.binpb testdata/schema/schema.binpb typescript/src/gen typescript/tests/gen)" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the results."; \
		git status --short -- proto/descriptor.binpb conformance/proto/descriptor.binpb go/gen go/tests/gen python/src/buf python/src/invariant/gen python/tests/proto/descriptor.binpb python/tests/proto/gen testdata/cdc/v2 testdata/data.schema.binpb testdata/openapi/descriptor.binpb testdata/openapi/gen/library/v1/library.proto testdata/schema/descriptor.binpb testdata/schema/schema.binpb typescript/src/gen typescript/tests/gen; \
		exit 1; \
	fi
	$(MAKE) openapi-codegen-check
