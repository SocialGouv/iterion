#!/usr/bin/env bash
# Run from the repository root; both callers use this generation recipe.
set -euo pipefail
spec_output=${1:-openapi.json}
types_output=${2:-studio/src/api/schema.ts}
case "$spec_output" in /*) ;; *) spec_output="$PWD/$spec_output" ;; esac
case "$types_output" in /*) ;; *) types_output="$PWD/$types_output" ;; esac
go run -mod=vendor ./cmd/iterion openapi -o "$spec_output"
corepack pnpm -C studio exec openapi-typescript "$spec_output" -o "$types_output"
