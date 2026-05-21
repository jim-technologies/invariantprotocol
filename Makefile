.PHONY: lint fmt test bench generate deps verify-generate breaking

BASE_REF ?= origin/main

lint:
	cd go && golangci-lint run ./...
	cd python && ruff check src/ tests/
	cd rust && cargo clippy --all-targets -- -D warnings
	cd proto && buf lint

fmt:
	cd go && gofmt -w . && golangci-lint run --fix ./...
	cd python && ruff format src/ tests/ && ruff check --fix src/ tests/
	cd rust && cargo fmt
	cd proto && buf format -w

test:
	cd go && go test ./...
	cd python && uv run python -m pytest tests/
	cd rust && cargo test

bench:
	cd go && go test -bench=. -benchtime=2s -run=^$$ ./...
	cd python && uv run python bench/bench.py
	cd rust && cargo bench --bench bench -- --warm-up-time 1 --measurement-time 2

generate:
	cd proto && buf generate

deps:
	cd go && go mod tidy
	cd rust && cargo update

breaking:
	cd proto && buf breaking --against "../.git#ref=$(BASE_REF),subdir=proto"

verify-generate:
	$(MAKE) generate
	@if [ -n "$$(git status --porcelain --untracked-files=all -- go/gen python/src/invariant/gen)" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the results."; \
		git status --short -- go/gen python/src/invariant/gen; \
		exit 1; \
	fi
