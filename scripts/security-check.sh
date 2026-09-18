#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

# Keep scanner versions explicit and run with the project's patched Go toolchain.
# CI checks out complete history so historical credentials also fail the build.
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
go run github.com/zricethezav/gitleaks/v8@v8.30.1 git --redact=100 --no-banner --log-opts=--all .
