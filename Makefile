# Robo-Taxi Telemetry — Build & Development Targets

BINARY      := telemetry-server
CMD_DIR     := ./cmd/telemetry-server
BIN_DIR     := ./bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE        ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS     := -s -w \
               -X main.version=$(VERSION) \
               -X main.commit=$(COMMIT) \
               -X main.date=$(DATE)

.PHONY: build test lint vet run clean proto help

## build: Compile the telemetry-server binary
build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD_DIR)

## test: Run all tests with race detector
test:
	go test -race -count=1 ./...

## lint: Run golangci-lint
lint:
	golangci-lint run

## vet: Run go vet
vet:
	go vet ./...

## run: Build and run the server
run: build
	$(BIN_DIR)/$(BINARY)

## proto: Generate Go types from Tesla protobuf definitions
proto:
	./scripts/generate-proto.sh

## clean: Remove build artifacts
clean:
	rm -rf $(BIN_DIR)
	go clean -cache -testcache

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed 's/^/  /'
