#!/usr/bin/env bash
# Update Formula/iterion.rb and Cask/iterion-desktop.rb to a new release.
#
# Usage: scripts/update-brew-tap.sh <version> <artifacts-dir>
#
# <version>       semver without the leading 'v' (e.g. 0.4.1)
# <artifacts-dir> directory containing the release artefacts:
#                   iterion-darwin-arm64, iterion-darwin-arm64.sha256, ...
#                   iterion-desktop-darwin-universal.zip[.sha256]
#
# Sidecar .sha256 files are preferred (they ship from the CLI release
# pipeline, see .github/workflows/release.yml). If absent, the script
# computes the hash from the binary directly — used for the desktop ZIP
# whose pipeline does not emit a .sha256 sidecar today.
#
# Missing artefacts are tolerated: e.g. running with only CLI binaries
# present updates Formula/ and leaves Cask/ untouched, and vice-versa.
# The final `git diff` line lets the CI step know whether anything moved.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 <version> <artifacts-dir>" >&2
  exit 2
fi

VERSION="$1"
ARTIFACTS="$2"

if [ ! -d "$ARTIFACTS" ]; then
  echo "error: artifacts dir not found: $ARTIFACTS" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FORMULA="$REPO_ROOT/Formula/iterion.rb"
CASK="$REPO_ROOT/Cask/iterion-desktop.rb"

# Read sha from <file>.sha256 if present, otherwise compute from <file>.
# Echoes the 64-char hex digest, or empty string if neither is available.
resolve_sha() {
  local path="$1"
  if [ -f "${path}.sha256" ]; then
    awk '{print $1; exit}' "${path}.sha256"
  elif [ -f "$path" ]; then
    sha256sum "$path" | awk '{print $1}'
  else
    echo ""
  fi
}

# --- CLI formula ---------------------------------------------------------

if [ -f "$FORMULA" ]; then
  SHA_DARWIN_ARM64="$(resolve_sha "$ARTIFACTS/iterion-darwin-arm64")"
  SHA_DARWIN_AMD64="$(resolve_sha "$ARTIFACTS/iterion-darwin-amd64")"
  SHA_LINUX_ARM64="$(resolve_sha "$ARTIFACTS/iterion-linux-arm64")"
  SHA_LINUX_AMD64="$(resolve_sha "$ARTIFACTS/iterion-linux-amd64")"

  if [ -n "$SHA_DARWIN_ARM64$SHA_DARWIN_AMD64$SHA_LINUX_ARM64$SHA_LINUX_AMD64" ]; then
    tmp="$(mktemp)"
    awk -v ver="$VERSION" \
        -v s_darm="$SHA_DARWIN_ARM64" \
        -v s_damd="$SHA_DARWIN_AMD64" \
        -v s_larm="$SHA_LINUX_ARM64" \
        -v s_lamd="$SHA_LINUX_AMD64" '
      function replace_sha(line, hash,    out) {
        if (hash == "") return line
        out = line
        sub(/sha256 "[^"]*"/, "sha256 \"" hash "\"", out)
        return out
      }
      # Single top-level version line (the formula has exactly one).
      /^[[:space:]]*version "[^"]*"$/ {
        sub(/version "[^"]*"/, "version \"" ver "\"")
        print
        next
      }
      # Track which platform block the next sha256 line belongs to by
      # sniffing the URL one line above.
      /url ".*iterion-darwin-arm64"/ { target = "darwin_arm64"; print; next }
      /url ".*iterion-darwin-amd64"/ { target = "darwin_amd64"; print; next }
      /url ".*iterion-linux-arm64"/  { target = "linux_arm64";  print; next }
      /url ".*iterion-linux-amd64"/  { target = "linux_amd64";  print; next }
      target != "" && /^[[:space:]]*sha256 "/ {
        if (target == "darwin_arm64") $0 = replace_sha($0, s_darm)
        else if (target == "darwin_amd64") $0 = replace_sha($0, s_damd)
        else if (target == "linux_arm64")  $0 = replace_sha($0, s_larm)
        else if (target == "linux_amd64")  $0 = replace_sha($0, s_lamd)
        target = ""
        print
        next
      }
      { print }
    ' "$FORMULA" > "$tmp"
    mv "$tmp" "$FORMULA"
    echo "updated Formula/iterion.rb to v${VERSION}"
  else
    echo "skipped Formula/iterion.rb (no CLI artefacts found in $ARTIFACTS)"
  fi
fi

# --- Desktop cask --------------------------------------------------------

if [ -f "$CASK" ]; then
  SHA_DESKTOP="$(resolve_sha "$ARTIFACTS/iterion-desktop-darwin-universal.zip")"

  if [ -n "$SHA_DESKTOP" ]; then
    tmp="$(mktemp)"
    awk -v ver="$VERSION" -v sha="$SHA_DESKTOP" '
      /^[[:space:]]*version "[^"]*"$/ {
        sub(/version "[^"]*"/, "version \"" ver "\"")
        print
        next
      }
      /^[[:space:]]*sha256 "[^"]*"$/ {
        sub(/sha256 "[^"]*"/, "sha256 \"" sha "\"")
        print
        next
      }
      { print }
    ' "$CASK" > "$tmp"
    mv "$tmp" "$CASK"
    echo "updated Cask/iterion-desktop.rb to v${VERSION}"
  else
    echo "skipped Cask/iterion-desktop.rb (no desktop ZIP found in $ARTIFACTS)"
  fi
fi

# --- Diff for CI logs ----------------------------------------------------

if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo
  echo "=== diff ==="
  git -C "$REPO_ROOT" --no-pager diff --no-color -- Formula/iterion.rb Cask/iterion-desktop.rb || true
fi
