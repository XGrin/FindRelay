GO ?= go
BINARY := findrelay
DIST_DIR := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt check clean

all: build

build:
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags='$(LDFLAGS)' -o $(DIST_DIR)/$(BINARY) ./cmd/$(BINARY)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

# Same gate as CI: checks only, never modifies files.
check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) build ./...
	CGO_ENABLED=1 $(GO) test -race ./...

clean:
	rm -rf $(DIST_DIR)
