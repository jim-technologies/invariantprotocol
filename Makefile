.DEFAULT_GOAL := help

.PHONY: help check version-check git-install-check lint fmt fmt-check test typecheck proto-comments public-surface audit bench generate deps verify-generate breaking

BASE_REF ?= origin/main

help: ## Show available make targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Single entry point: format-check + lint + typecheck + tests across all four
# languages (Go, Python, Rust, TypeScript) plus proto/comment gates. CI runs
# `flox activate -- make check` so contributors and CI hit the identical
# toolchain and gates.
check: version-check fmt-check lint typecheck proto-comments public-surface test ## Run the canonical validation gate.

node_modules/.package-lock.json: package-lock.json package.json
	npm ci

version-check: ## Verify every language package uses the root VERSION.
	python3 scripts/check_versions.py

git-install-check: ## Install every language package from the current Git commit.
	scripts/check_git_installs.sh

fmt-check: ## Verify formatting without modifying files.
	test -z "$$(gofmt -l go)" || { echo "gofmt: files need formatting:"; gofmt -l go; exit 1; }
	cd python && ruff format --check src/ tests/ ../scripts/
	cd rust && cargo fmt --check
	cd proto && buf format --diff --exit-code

lint: ## Run Go, Python, Rust, and proto linters.
	golangci-lint run ./...
	cd python && ruff check src/ tests/ ../scripts/
	cd rust && cargo clippy --all-targets -- -D warnings
	cd proto && buf lint

typecheck: node_modules/.package-lock.json ## Run Python and TypeScript static type checks.
	cd python && uv run ty check
	npm run lint

proto-comments: ## Verify projected proto comments are complete.
	cd python && uv run invariant-check-proto-comments tests/proto/descriptor.binpb

public-surface: ## Scan OSS-facing files for private/product-specific references.
	python3 scripts/check_public_surface.py

audit: ## Scan Python dependencies for known vulnerabilities.
	cd python && uv run pip-audit

fmt: ## Format code and apply safe linter fixes.
	gofmt -w go && golangci-lint run --fix ./...
	cd python && ruff format src/ tests/ ../scripts/ && ruff check --fix src/ tests/ ../scripts/
	cd rust && cargo fmt
	cd proto && buf format -w

test: node_modules/.package-lock.json ## Run Go, Python, Rust, and TypeScript tests.
	go test ./...
	cd python && uv run python -m pytest tests/
	cd rust && cargo test
	npm test

bench: ## Run Go, Python, and Rust benchmarks.
	go test -bench=. -benchtime=2s -run=^$$ ./...
	cd python && uv run python bench/bench.py
	cd rust && cargo bench --bench bench -- --warm-up-time 1 --measurement-time 2

generate: ## Regenerate protobuf stubs.
	cd proto && buf generate
	cd python/tests/proto && buf build -o descriptor.binpb
	cd python/tests/proto && buf generate
	cd python/tests/proto && buf generate --template buf.validate.gen.yaml

deps: ## Tidy/update language dependency lockfiles.
	go get -u all && go mod tidy
	cd python && uv lock --upgrade
	cd rust && cargo update
	npm update
	cd python/tests/proto && buf dep update

breaking: ## Check proto breaking changes against BASE_REF.
	cd proto && buf breaking --against "../.git#ref=$(BASE_REF),subdir=proto"

verify-generate: ## Verify generated protobuf stubs are committed.
	$(MAKE) generate
	@if [ -n "$$(git status --porcelain --untracked-files=all -- go/gen go/tests/gen python/src/buf python/src/invariant/gen python/tests/proto/descriptor.binpb python/tests/proto/gen)" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the results."; \
		git status --short -- go/gen go/tests/gen python/src/buf python/src/invariant/gen python/tests/proto/descriptor.binpb python/tests/proto/gen; \
		exit 1; \
	fi
