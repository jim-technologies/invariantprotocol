.DEFAULT_GOAL := help

.PHONY: help check quality version-check parity parity-release git-install-check integration lint fmt fmt-check test test-go test-python test-rust test-typescript typecheck proto-comments public-surface security bench generate deps verify-generate breaking

BASE_REF ?= origin/main

help: ## Show available make targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Single local entry point. CI runs quality and language tests as separate jobs
# so failures are easy to identify and the four test suites execute in parallel.
check: quality test ## Run deterministic quality checks and all tests.

quality: version-check parity fmt-check lint typecheck proto-comments public-surface ## Run formatting, lint, type, schema, and policy checks.

node_modules/.package-lock.json: package-lock.json package.json
	npm ci --ignore-scripts

version-check: ## Verify every language package uses the root VERSION.
	python3 scripts/check_versions.py

parity: ## Validate and report the cross-language feature contract.
	python3 scripts/check_feature_parity.py

parity-release: ## Reject a release while any core feature lacks four-language support.
	python3 scripts/check_feature_parity.py --release

git-install-check: ## Install every language package from the current Git commit.
	scripts/check_git_installs.sh

integration: git-install-check ## Exercise downstream Git installation for every language.

fmt-check: ## Verify formatting without modifying files.
	test -z "$$(gofmt -l go)" || { echo "gofmt: files need formatting:"; gofmt -l go; exit 1; }
	cd python && ruff format --check src/ tests/ ../scripts/
	cd rust && cargo fmt --all --check
	cd proto && buf format --diff --exit-code
	cd conformance/proto && buf format --diff --exit-code

lint: ## Run Go, Python, Rust, and proto linters.
	actionlint
	golangci-lint run ./...
	cd python && ruff check src/ tests/ ../scripts/
	cd rust && cargo clippy --workspace --all-targets --locked -- -D warnings
	cd proto && buf lint
	cd conformance/proto && buf lint

typecheck: node_modules/.package-lock.json ## Run Python and TypeScript static type checks.
	cd python && uv run ty check
	npm run lint

proto-comments: ## Verify projected proto comments are complete.
	cd python && uv run invariant-check-proto-comments tests/proto/descriptor.binpb

public-surface: ## Scan OSS-facing files for private/product-specific references.
	python3 scripts/check_public_surface.py

security: node_modules/.package-lock.json ## Scan secrets and verify/audit every dependency graph.
	gitleaks git --no-banner --redact .
	go mod verify
	govulncheck -test ./...
	cd python && uv lock --check && uv run pip-audit
	npm audit signatures
	npm audit --audit-level=moderate
	cd rust && cargo fetch --locked && cargo audit

fmt: ## Format code and apply safe linter fixes.
	gofmt -w go && golangci-lint run --fix ./...
	cd python && ruff format src/ tests/ ../scripts/ && ruff check --fix src/ tests/ ../scripts/
	cd rust && cargo fmt --all
	cd proto && buf format -w
	cd conformance/proto && buf format -w

test: test-go test-python test-rust test-typescript ## Run all language test suites.

test-go: ## Run Go unit and transport-integration tests.
	go test ./...

test-python: ## Run Python unit and transport-integration tests.
	cd python && uv run python -m pytest tests/

test-rust: ## Run Rust unit and transport-integration tests.
	cd rust && cargo test --workspace --all-targets --locked

test-typescript: node_modules/.package-lock.json ## Run TypeScript unit and transport-integration tests.
	npm test

bench: ## Run Go, Python, and Rust benchmarks.
	go test -bench=. -benchtime=2s -run=^$$ ./...
	cd python && uv run python bench/bench.py
	cd rust && cargo bench --locked --bench bench -- --warm-up-time 1 --measurement-time 2

generate: node_modules/.package-lock.json ## Regenerate protobuf stubs.
	# Remove only configured generator outputs. Keep Python package markers and
	# hand-written files; this makes deleted/renamed protos delete stale bindings.
	find go/gen go/tests/gen -type f -name '*.pb.go' -delete
	find python/src/invariant/gen python/src/buf python/tests/proto/gen -type f \( -name '*_pb2.py' -o -name '*_pb2_grpc.py' \) -delete
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
	go run ./go/cmd/invariant-schema compile --descriptor python/tests/proto/descriptor.binpb --message data.v1.CanonicalRecord --message data.v1.Proto2Record --output testdata/data.schema.binpb

deps: ## Tidy/update language dependency lockfiles.
	flox upgrade
	go get -u all && go mod tidy
	cd python && uv lock --upgrade
	cd rust && cargo update
	npm update
	cd python/tests/proto && buf dep update

breaking: ## Check proto breaking changes against BASE_REF.
	cd proto && buf breaking --against "../.git#ref=$(BASE_REF),subdir=proto"

verify-generate: ## Verify generated protobuf stubs are committed.
	$(MAKE) generate
	@if [ -n "$$(git status --porcelain --untracked-files=all -- proto/descriptor.binpb conformance/proto/descriptor.binpb go/gen go/tests/gen python/src/buf python/src/invariant/gen python/tests/proto/descriptor.binpb python/tests/proto/gen testdata/data.schema.binpb typescript/src/gen typescript/tests/gen)" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the results."; \
		git status --short -- proto/descriptor.binpb conformance/proto/descriptor.binpb go/gen go/tests/gen python/src/buf python/src/invariant/gen python/tests/proto/descriptor.binpb python/tests/proto/gen testdata/data.schema.binpb typescript/src/gen typescript/tests/gen; \
		exit 1; \
	fi
