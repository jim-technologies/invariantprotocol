.DEFAULT_GOAL := help

.PHONY: help check lint fmt fmt-check test typecheck proto-comments audit bench generate deps verify-generate breaking

BASE_REF ?= origin/main

help: ## Show available make targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Single entry point: format-check + lint + typecheck + tests across all four
# languages (Go, Python, Rust, TypeScript) plus proto/comment gates. CI runs
# `flox activate -- make check` so contributors and CI hit the identical
# toolchain and gates.
check: fmt-check lint typecheck proto-comments test ## Run the canonical validation gate.

typescript/node_modules/.package-lock.json: typescript/package-lock.json typescript/package.json
	cd typescript && npm ci

fmt-check: ## Verify formatting without modifying files.
	cd go && test -z "$$(gofmt -l .)" || { echo "gofmt: files need formatting:"; gofmt -l .; exit 1; }
	cd python && ruff format --check src/ tests/
	cd rust && cargo fmt --check
	cd proto && buf format --diff --exit-code

lint: ## Run Go, Python, Rust, and proto linters.
	cd go && golangci-lint run ./...
	cd python && ruff check src/ tests/
	cd rust && cargo clippy --all-targets -- -D warnings
	cd proto && buf lint

typecheck: typescript/node_modules/.package-lock.json ## Run Python and TypeScript static type checks.
	cd python && uv run ty check
	cd typescript && npm run lint

proto-comments: ## Verify projected proto comments are complete.
	cd python && uv run invariant-check-proto-comments tests/proto/descriptor.binpb

audit: ## Scan Python dependencies for known vulnerabilities.
	# Python dependency CVE scan. The two ignored advisories are dev-only test
	# tooling (pytest + its pygments) with no resolvable fix yet; neither ships
	# in the published wheel. Runtime deps (incl. idna via httpx) are kept fixed.
	cd python && uv run pip-audit \
		--ignore-vuln CVE-2026-4539 --ignore-vuln CVE-2025-71176

fmt: ## Format code and apply safe linter fixes.
	cd go && gofmt -w . && golangci-lint run --fix ./...
	cd python && ruff format src/ tests/ && ruff check --fix src/ tests/
	cd rust && cargo fmt
	cd proto && buf format -w

test: typescript/node_modules/.package-lock.json ## Run Go, Python, Rust, and TypeScript tests.
	cd go && go test ./...
	cd python && uv run python -m pytest tests/
	cd rust && cargo test
	cd typescript && npm test

bench: ## Run Go, Python, and Rust benchmarks.
	cd go && go test -bench=. -benchtime=2s -run=^$$ ./...
	cd python && uv run python bench/bench.py
	cd rust && cargo bench --bench bench -- --warm-up-time 1 --measurement-time 2

generate: ## Regenerate protobuf stubs.
	cd proto && buf generate

deps: ## Tidy/update language dependency lockfiles.
	cd go && go mod tidy
	cd rust && cargo update

breaking: ## Check proto breaking changes against BASE_REF.
	cd proto && buf breaking --against "../.git#ref=$(BASE_REF),subdir=proto"

verify-generate: ## Verify generated protobuf stubs are committed.
	$(MAKE) generate
	@if [ -n "$$(git status --porcelain --untracked-files=all -- go/gen python/src/invariant/gen)" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the results."; \
		git status --short -- go/gen python/src/invariant/gen; \
		exit 1; \
	fi
