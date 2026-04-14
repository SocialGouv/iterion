#!/bin/sh
# Deterministic parity scan: counts Go/Rust files and lines
# Usage: parity_scan.sh <go_repo_path> <rust_repo_path>
set -e

GO_REPO="$1"
RUST_REPO="$2"

GO_SRC=$(find "$GO_REPO" -name '*.go' -not -path '*/vendor/*' -not -name '*_test.go' | wc -l)
GO_TEST=$(find "$GO_REPO" -name '*_test.go' -not -path '*/vendor/*' | wc -l)
GO_LINES=$(find "$GO_REPO" -name '*.go' -not -path '*/vendor/*' -not -name '*_test.go' -exec cat {} + 2>/dev/null | wc -l)
GO_TLINES=$(find "$GO_REPO" -name '*_test.go' -not -path '*/vendor/*' -exec cat {} + 2>/dev/null | wc -l)
GO_PKGS=$(find "$GO_REPO" -name '*.go' -not -path '*/vendor/*' -exec dirname {} \; | sort -u | tr '\n' ', ')

RS_SRC=$(find "$RUST_REPO/crates" -name '*.rs' 2>/dev/null | wc -l)
RS_LINES=$(find "$RUST_REPO/crates" -name '*.rs' -exec cat {} + 2>/dev/null | wc -l)
RS_CRATES=$(ls "$RUST_REPO/crates/" 2>/dev/null | tr '\n' ', ')

if [ "$RS_LINES" -gt 0 ]; then
  RATIO=$((GO_LINES * 100 / RS_LINES))
else
  RATIO=0
fi

printf '{"go_source_files":%d,"go_test_files":%d,"go_source_lines":%d,"go_test_lines":%d,"go_packages":"%s","rust_source_files":%d,"rust_source_lines":%d,"rust_crates":"%s","line_ratio_percent":"%d%%"}' \
  "$GO_SRC" "$GO_TEST" "$GO_LINES" "$GO_TLINES" "$GO_PKGS" "$RS_SRC" "$RS_LINES" "$RS_CRATES" "$RATIO"
