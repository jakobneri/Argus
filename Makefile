# argus — Makefile
#
# Assumptions (see README):
#   - CGO is disabled everywhere; all dependencies are pure Go.
#   - Proto codegen uses buf (pure Go) with protoc-gen-go / protoc-gen-go-grpc.
#     Install the tools once with `make tools`.

GO        ?= go
GOBIN     := $(shell $(GO) env GOPATH)/bin
BINDIR    := bin
BINARIES  := argusd argus argus-mcp

export CGO_ENABLED = 0

.PHONY: all build cross lint test proto fmt tools clean

all: build

## build: build all three binaries for the host platform into ./bin
build:
	@mkdir -p $(BINDIR)
	$(GO) build -trimpath -o $(BINDIR)/ ./cmd/...

## cross: statically linked linux/arm64 build (Raspberry Pi 5)
cross:
	GOOS=linux GOARCH=arm64 $(GO) build -trimpath ./...
	@mkdir -p $(BINDIR)/linux-arm64
	GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o $(BINDIR)/linux-arm64/ ./cmd/...

## lint: gofumpt formatting check + golangci-lint
lint:
	@test -z "$$($(GOBIN)/gofumpt -l . 2>/dev/null | grep -v '\.pb\.go')" || \
		{ echo 'gofumpt: files need formatting (run make fmt):'; $(GOBIN)/gofumpt -l . | grep -v '\.pb\.go'; exit 1; }
	golangci-lint run ./...

## test: run all tests
test:
	$(GO) test ./...

## proto: regenerate gRPC code from proto/ via buf
proto:
	$(GOBIN)/buf lint
	$(GOBIN)/buf generate

## fmt: format all Go sources with gofumpt
fmt:
	$(GOBIN)/gofumpt -w .

## tools: install codegen + formatting tools into GOPATH/bin
tools:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	$(GO) install github.com/bufbuild/buf/cmd/buf@v1.47.2
	$(GO) install mvdan.cc/gofumpt@latest

clean:
	rm -rf $(BINDIR)
