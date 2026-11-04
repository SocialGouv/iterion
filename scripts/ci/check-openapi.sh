#!/usr/bin/env bash
# Compare private generated files with the index without writing the checkout.
set -euo pipefail
generated_dir="$(mktemp -d "${TMPDIR:-/tmp}/iterion-openapi-check.XXXXXXXX")"
cleanup() {
  local status=$?
  trap - EXIT
  trap 'status=129' HUP
  trap 'status=130' INT
  trap 'status=143' TERM
  if ! (trap '' HUP INT TERM; rm -rf -- "$generated_dir"); then
    [ "$status" -ne 0 ] || status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$generated_dir/indexed" "$generated_dir/generated"
git show :openapi.json > "$generated_dir/indexed/openapi.json"
git show :studio/src/api/schema.ts > "$generated_dir/indexed/schema.ts"
bash scripts/ci/generate-openapi.sh "$generated_dir/generated/openapi.json" "$generated_dir/generated/schema.ts"
if git diff --no-index --exit-code --stat -- "$generated_dir/indexed" "$generated_dir/generated"; then
  exit 0
else
  status=$?
  echo "::error::OpenAPI spec / client drift — run 'task openapi:gen' and commit the result."
  exit "$status"
fi
