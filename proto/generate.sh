#!/usr/bin/env bash
# Regenerate Go code from the .proto files. Invoked by `make generate`.
#
# Requires protoc, protoc-gen-go and protoc-gen-go-grpc on PATH. The generated
# files are committed, and `make verify-generated` fails CI if they are stale.
set -euo pipefail
cd "$(dirname "$0")/.."

PATH="$(go env GOPATH)/bin:$PATH"
for tool in protoc protoc-gen-go protoc-gen-go-grpc; do
  command -v "$tool" >/dev/null || { echo "missing $tool on PATH" >&2; exit 1; }
done

protoc \
  --proto_path=proto \
  --go_out=. --go_opt=module=github.com/mcpdoll/mcpdoll \
  --go-grpc_out=. --go-grpc_opt=module=github.com/mcpdoll/mcpdoll \
  proto/mcpdoll/snapshot/v1/*.proto \
  proto/mcpdoll/plugin/v1/*.proto

echo "generated protobuf Go code"
