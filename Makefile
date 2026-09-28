.DEFAULT_GOAL := help

.PHONY: help help-all build validate validate-static release release-test version-check parity parity-release git-install-check connect-interop postgres-integration clickhouse-integration lance-integration data-integration integration lint fmt fmt-check go-mod-check test test-go race-go test-python test-rust test-typescript coverage coverage-go coverage-python coverage-rust coverage-typescript typecheck proto-comments public-surface security bench generate openapi-codegen-check deps verify-generate breaking

help: ## One-screen help (make help-all for every target)
	@echo "Daily:"
	@echo "  make fmt        format code, apply safe linter fixes"
	@echo "  make generate   regenerate committed build artifacts"
	@echo "  make test       run all language test suites"
	@echo "  make validate   the full offline gate"
	@echo "  make release    tag and push v<VERSION> (maintainers, after CI passes on main)"
	@echo ""
	@echo "Everything else: make help-all"

help-all: ## Every target with its description
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/'

build: node_modules/.package-lock.json ## Build every language package and command.
	GOFLAGS=-mod=readonly go build ./...
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; cd python && uv build --out-dir "$$tmp"
	cd rust && cargo build --workspace --locked
	npm run build

# The single gate (see MAKEFILE-CONTRACT.md). CI runs exactly
# `flox activate -- make validate` in one job, so CI cannot drift from it.
validate: validate-static verify-generate coverage race-go ## Run the full gate: static checks, generated-code staleness, coverage-gated tests, and the Go race detector.

validate-static: version-check parity fmt-check lint typecheck proto-comments breaking release-test public-surface go-mod-check ## Run the static slice of validate: formatting, lint, type, schema, breaking-change, release-guard, and policy checks.

node_modules/.package-lock.json: package-lock.json package.json
	npm ci --ignore-scripts

version-check: ## Verify every language package uses the root VERSION.
	python3 scripts/check_versions.py

parity: ## Validate and report the cross-language feature contract.
	python3 scripts/check_feature_parity.py

parity-release: ## Reject a release while any core feature lacks four-language support.
	python3 scripts/check_feature_parity.py --release

release: ## Create and push the annotated v<VERSION> tag after the release guards (maintainers only).
	scripts/release.sh

release-test: ## Pin the release guards and the tag push in throwaway Git fixtures.
	scripts/release_test.sh

git-install-check: ## Install every language package from the current Git commit.
	scripts/check_git_installs.sh

connect-interop: node_modules/.package-lock.json ## Exercise Go, Python, and Rust HTTP projections with Connect-ES.
	scripts/connect_interop.sh

postgres-integration: ## Apply and round-trip generated PostgreSQL through Atlas.
	scripts/check_postgres_atlas.sh

clickhouse-integration: ## Apply generated declarations and round-trip values through ClickHouse.
	scripts/check_clickhouse.sh

lance-integration: ## Exercise invariant-generated Arrow data through a local LanceDB lifecycle.
	cd python && uv run --locked python -m pytest tests/test_data_lance.py

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
	cd python && uv run --locked mypy
	npm run typecheck

proto-comments: ## Verify projected proto comments are complete.
	cd python && uv run --locked invariant-check-proto-comments tests/proto/descriptor.binpb

public-surface: ## Guard the public surface: tracked content, paths, and unpushed commit messages.
	scripts/public-surface-check
	scripts/public-surface-check-test

go-mod-check: ## Verify Go dependency metadata is canonical and complete.
	go mod tidy -diff

security: node_modules/.package-lock.json ## Scan secrets and verify/audit every dependency graph.
	gitleaks git --no-banner --redact .
	go mod verify
	GOFLAGS=-mod=readonly govulncheck -test ./...
	cd python && uv lock --check && uv run --locked pip-audit
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
	cd python && uv run --locked python -m pytest tests/

test-rust: ## Run Rust unit and transport-integration tests.
	cd rust && cargo test --workspace --all-targets --locked

test-typescript: node_modules/.package-lock.json ## Run TypeScript unit and transport-integration tests.
	npm test

coverage: coverage-go coverage-python coverage-rust coverage-typescript ## Run tests with maintained coverage floors.

coverage-go: ## Run Go tests and enforce authored-code statement coverage.
	scripts/coverage_go.sh

coverage-python: ## Run Python tests with branch coverage.
	cd python && uv run --locked python -m pytest --cov=invariant --cov-branch --cov-report=term-missing tests/

coverage-rust: ## Run Rust tests with LLVM source coverage.
	cd rust && LLVM_COV="$$(command -v llvm-cov)" LLVM_PROFDATA="$$(command -v llvm-profdata)" cargo llvm-cov --workspace --locked --ignore-filename-regex '/target/' --fail-under-lines 80

coverage-typescript: node_modules/.package-lock.json ## Run TypeScript tests with V8 coverage.
	npm run test:coverage

bench: ## Run Go, Python, and Rust benchmarks.
	GOFLAGS=-mod=readonly go test -bench=. -benchtime=2s -run=^$$ ./...
	cd python && uv run --locked python bench/bench.py
	cd rust && cargo bench --locked --bench bench -- --warm-up-time 1 --measurement-time 2

generate: node_modules/.package-lock.json ## Regenerate committed build artifacts.
	scripts/generate.sh

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

breaking: ## Check proto breaking changes against the previous release tag (a pull request's base branch in CI).
	scripts/check_breaking_test.sh
	scripts/check_breaking.sh

verify-generate: node_modules/.package-lock.json ## Verify generated build artifacts are committed, then compile the imported OpenAPI fixture.
	scripts/verify_generate.sh
	scripts/check_openapi_codegen.sh
