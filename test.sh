#!/usr/bin/env bash
# Kilter local CI: run before every commit. Fails fast.
set -euo pipefail
cd "$(dirname "$0")"

echo "==> gofmt"
unformatted=$(gofmt -l . | grep -v '^tools/' || true)
if [[ -n "$unformatted" ]]; then
  echo "gofmt needed on:"; echo "$unformatted"; exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> go test -race"
go test -race -short -count=1 ./...

echo "==> scale soaks (no race)"
go test -count=1 ./test/scale/

echo "==> go build"
go build ./...

echo "OK"
