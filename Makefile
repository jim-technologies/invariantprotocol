.DEFAULT_GOAL := help

.PHONY: help check quality version-check parity parity-release git-install-check data-integration integration lint fmt fmt-check test test-go test-python test-rust test-typescript coverage coverage-go coverage-python coverage-rust coverage-typescript typecheck proto-comments public-surface security bench generate deps verify-generate breaking

BASE_REF ?= origin/main

help: ## Show available make targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Single local entry point. CI runs quality and language coverage as separate
# jobs so failures are easy to identify and the four suites execute in parallel.
check: quality coverage ## Run deterministic quality checks, tests, and coverage gates.

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

data-integration: ## Apply and round-trip generated SQL through PostgreSQL and Atlas.
	scripts/check_postgres_atlas.sh

integration: git-install-check data-integration ## Exercise Git installation and external data boundaries.

fmt-check: node_modules/.package-lock.json ## Verify formatting without modifying files.
	test -z "$$(gofmt -l go)" || { echo "gofmt: files need formatting:"; gofmt -l go; exit 1; }
	cd python && ruff format --check src/ tests/ ../scripts/
	cd rust && cargo fmt --all --check
	cd proto && buf format --diff --exit-code
	cd conformance/proto && buf format --diff --exit-code
	npm run format:check

lint: node_modules/.package-lock.json ## Run Go, Python, Rust, and proto linters.
	actionlint
	shellcheck scripts/*.sh
	golangci-lint run ./...
	cd python && ruff check src/ tests/ ../scripts/
	cd rust && cargo clippy --workspace --all-targets --locked -- -D warnings
	cd proto && buf lint
	cd conformance/proto && buf lint
	npm run lint

typecheck: node_modules/.package-lock.json ## Run Python and TypeScript static type checks.
	cd python && uv run ty check
	npm run typecheck

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

fmt: node_modules/.package-lock.json ## Format code and apply safe linter fixes.
	gofmt -w go && golangci-lint run --fix ./...
	cd python && ruff format src/ tests/ ../scripts/ && ruff check --fix src/ tests/ ../scripts/
	cd rust && cargo fmt --all
	cd proto && buf format -w
	cd conformance/proto && buf format -w
	npm run format

test: test-go test-python test-rust test-typescript ## Run all language test suites.

test-go: ## Run Go unit and transport-integration tests.
	go test ./...

test-python: ## Run Python unit and transport-integration tests.
	cd python && uv run python -m pytest tests/

test-rust: ## Run Rust unit and transport-integration tests.
	cd rust && cargo test --workspace --all-targets --locked

test-typescript: node_modules/.package-lock.json ## Run TypeScript unit and transport-integration tests.
	npm test

coverage: coverage-go coverage-python coverage-rust coverage-typescript ## Run tests with maintained coverage floors.

coverage-go: ## Run Go tests and enforce authored-code statement coverage.
	@set -eu; \
	all_packages="$$(go list ./go/...)"; \
	packages="$$(printf '%s\n' "$$all_packages" | grep -Ev '/go/(gen/|tests/(gen|manual)$$)')"; \
	profile="$$(mktemp)"; \
	trap 'rm -f "$$profile"' EXIT; \
	go test -count=1 -covermode=atomic -coverprofile="$$profile" $$packages; \
	total="$$(go tool cover -func="$$profile" | awk '/^total:/ {gsub("%", "", $$3); print $$3}')"; \
	awk -v total="$$total" 'BEGIN { printf "Go authored statement coverage: %.1f%% (required: 80.0%%)\n", total; exit !(total >= 80.0) }'

coverage-python: ## Run Python tests with branch coverage.
	cd python && uv run python -m pytest --cov=invariant --cov-branch --cov-report=term-missing tests/

coverage-rust: ## Run Rust tests with LLVM source coverage.
	cd rust && LLVM_COV="$$(command -v llvm-cov)" LLVM_PROFDATA="$$(command -v llvm-profdata)" cargo llvm-cov --workspace --locked --ignore-filename-regex '/target/' --fail-under-lines 80

coverage-typescript: node_modules/.package-lock.json ## Run TypeScript tests with V8 coverage.
	npm run test:coverage

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
