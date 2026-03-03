.PHONY: lint fmt test generate deps

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
