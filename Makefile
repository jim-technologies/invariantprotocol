.PHONY: lint fmt test generate deps verify-generate

lint:
	cd go && golangci-lint run ./...
	cd python && ruff check src/ tests/
	cd proto && buf lint

fmt:
	cd go && gofmt -w . && golangci-lint run --fix ./...
	cd python && ruff format src/ tests/ && ruff check --fix src/ tests/
	cd proto && buf format -w

test:
	cd go && go test ./...
	cd python && uv run python -m pytest tests/

generate:
	cd proto && buf generate

deps:
	cd go && go mod tidy

verify-generate:
	$(MAKE) generate
	@if [ -n "$$(git status --porcelain --untracked-files=all)" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the results."; \
		git status --short; \
		exit 1; \
	fi
