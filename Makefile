GO ?= go

.PHONY: gate build vet fmt-check tidy-check test lint boundaries

# The green gate: every increment ends with this passing (docs/adr/ADR-009).
gate: build vet fmt-check tidy-check test boundaries lint

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tidy-check:
	$(GO) mod tidy -diff

test:
	$(GO) test -race -shuffle=on -count=1 ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed locally (make lint-install); CI runs it"; \
	fi

lint-install:
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
		| sh -s -- -b $$($(GO) env GOPATH)/bin latest

boundaries:
	scripts/check-boundaries.sh --self-test
	scripts/check-boundaries.sh
