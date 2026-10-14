#!/usr/bin/env bash
# dev.sh - local development entry point.
#
# Usage: scripts/dev.sh <test|lint|build|bench|fmt>
#
# Requires a Go toolchain on PATH.
set -euo pipefail

cd "$(dirname "$0")/.."

usage() {
  echo "usage: scripts/dev.sh <test|lint|build|bench|fmt>" >&2
  exit 2
}

cmd="${1:-}"
case "$cmd" in
  test)
    go test -race -count=1 ./...
    ;;
  lint)
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then
      echo "gofmt -l lists unformatted files:" >&2
      echo "$unformatted" >&2
      exit 1
    fi
    go vet ./...
    ;;
  build)
    go build ./...
    ;;
  bench)
    go test -run '^$' -bench . ./cmd/...
    ;;
  fmt)
    gofmt -w .
    ;;
  *)
    usage
    ;;
esac
