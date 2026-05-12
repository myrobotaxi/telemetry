#!/usr/bin/env bash
# generate-proto.sh — Generate Go types from vendored Tesla proto files.
#
# Prerequisites:
#   brew install protobuf
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#
# Usage:
#   ./scripts/generate-proto.sh

set -euo pipefail

# Ensure GOPATH/bin is on PATH for protoc-gen-go.
GOBIN="$(go env GOPATH)/bin"
export PATH="${GOBIN}:${PATH}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROTO_DIR="${REPO_ROOT}/internal/telemetry/proto/tesla"
OUT_DIR="${REPO_ROOT}/internal/telemetry/proto/tesla"

# Verify tools are available.
if ! command -v protoc &>/dev/null; then
    echo "error: protoc not found. Install with: brew install protobuf" >&2
    exit 1
fi

if ! command -v protoc-gen-go &>/dev/null; then
    echo "error: protoc-gen-go not found. Install with: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest" >&2
    exit 1
fi

echo "Generating Go code from Tesla proto files..."
echo "  Proto dir: ${PROTO_DIR}"
echo "  Output dir: ${OUT_DIR}"

# Remove previously generated files to avoid stale output.
rm -f "${OUT_DIR}"/*.pb.go

# Generate Go types for each proto file.
protoc \
    --proto_path="${PROTO_DIR}" \
    --go_out="${OUT_DIR}" \
    --go_opt=module=github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla \
    "${PROTO_DIR}"/*.proto

# Count generated files.
count=$(find "${OUT_DIR}" -name '*.pb.go' | wc -l | tr -d ' ')
echo "Generated ${count} Go file(s) in ${OUT_DIR}"
