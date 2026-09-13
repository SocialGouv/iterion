#!/usr/bin/env bash
# Run through devbox. The old source and both executables are disposable test
# artifacts; the checkout and its unrelated worktrees remain untouched.
set -euo pipefail

ports_legacy_base=3872f9dd1d1cbf9c18ed338c66c9f55aaaea3fcc
ports_fixture_output=${1:?usage: build-port-legacy-fixture.sh OUTPUT_DIRECTORY}
ports_repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
case "$(go env GOVERSION)" in
  go1.26.*) ;;
  *) echo 'Build this compatibility fixture through the repository Go 1.26 devbox environment.' >&2; exit 1 ;;
esac
mkdir -p "$ports_fixture_output"
ports_fixture_output=$(cd "$ports_fixture_output" && pwd)
ports_fixture_source=$(mktemp -d "$ports_fixture_output/source.XXXXXXXX")
trap 'rm -rf "$ports_fixture_source"' EXIT
if [[ -n "${PORTS_LEGACY_SOURCE_ARCHIVE:-}" ]]; then
  # A container may mount the task checkout without its parent .git. The
  # host can supply an archive produced by git archive of this exact commit.
  [[ "$(git get-tar-commit-id < "$PORTS_LEGACY_SOURCE_ARCHIVE")" == "$ports_legacy_base" ]]
  tar -x -f "$PORTS_LEGACY_SOURCE_ARCHIVE" -C "$ports_fixture_source"
else
  git -C "$ports_repo_dir" cat-file -e "${ports_legacy_base}^{commit}"
  git -C "$ports_repo_dir" archive "$ports_legacy_base" | tar -x -C "$ports_fixture_source"
fi
mkdir -p "$ports_fixture_source/cmd/ports-legacy-probe"
cp "$ports_repo_dir/pkg/store/storetest/testdata/legacyprobe/main.go" "$ports_fixture_source/cmd/ports-legacy-probe/main.go"
ports_fixture_ldflags="-X github.com/SocialGouv/iterion/pkg/internal/appinfo.Commit=$ports_legacy_base"
go build -C "$ports_fixture_source" -mod=vendor -buildvcs=false -ldflags "$ports_fixture_ldflags" -o "$ports_fixture_output/iterion-legacy" ./cmd/iterion
go build -C "$ports_fixture_source" -mod=vendor -buildvcs=false -ldflags "$ports_fixture_ldflags" -o "$ports_fixture_output/probe-legacy" ./cmd/ports-legacy-probe
printf '%s\n' "$ports_legacy_base" > "$ports_fixture_output/source-commit.txt"
sha256sum "$ports_fixture_output/iterion-legacy" "$ports_fixture_output/probe-legacy" > "$ports_fixture_output/binaries.sha256"
printf 'Legacy source: %s\nCLI: %s/iterion-legacy\nProbe: %s/probe-legacy\n' "$ports_legacy_base" "$ports_fixture_output" "$ports_fixture_output"
