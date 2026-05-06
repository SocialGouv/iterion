#!/usr/bin/env bash
#
# Build the desktop release manifest from a directory of artefacts. Each
# entry's URL is the github releases /download/v<VERSION>/<filename> path
# (assembled from VERSION + the filename), and the script signs the
# manifest itself with the same Ed25519 key sign-release.sh used.
#
# Usage:
#   VERSION=v0.4.0 \
#   UPDATER_ED25519_PRIVATE=$(cat ~/.iterion-keys/updater_ed25519.pem) \
#     ./scripts/desktop/generate-manifest.sh path/to/artefacts/
#
# Outputs <dir>/iterion-desktop-manifest.json and .sig.

set -euo pipefail

DIR="${1:?usage: generate-manifest.sh <dir>}"
VERSION="${VERSION:?VERSION env var required}"
RELEASE_BASE_URL="https://github.com/SocialGouv/iterion/releases/download/${VERSION}"
MANIFEST="$DIR/iterion-desktop-manifest.json"

# Map each artefact to its (GOOS/GOARCH) key in the manifest.
declare -A artifacts

for f in "$DIR"/iterion-desktop-*; do
  base="$(basename "$f")"
  case "$base" in
    *.sig|*.sha256) continue ;;
    *manifest*) continue ;;
    # Linux ships an AppImage (the auto-updater target), a .tar.gz
    # containing the raw binary, and a .deb for Debian/Ubuntu/Mint.
    # Only the AppImage goes into the manifest — the auto-updater can
    # swap an AppImage in place safely, but replacing a system-
    # installed binary or apt-managed package would race against the
    # local package manager.
    iterion-desktop-linux-*.tar.gz) continue ;;
    iterion-desktop-linux-*.deb)    continue ;;
  esac
  # The auto-updater looks up artefacts by `runtime.GOOS+"/"+runtime.GOARCH`.
  # darwin/universal must be exposed under BOTH darwin/amd64 and
  # darwin/arm64 so Intel Macs and Apple Silicon Macs each find a
  # match — the same lipo'd .app file serves both.
  keys=()
  case "$base" in
    iterion-desktop-darwin-universal*) keys=("darwin/amd64" "darwin/arm64") ;;
    iterion-desktop-darwin-arm64*)     keys=("darwin/arm64") ;;
    iterion-desktop-darwin-amd64*)     keys=("darwin/amd64") ;;
    iterion-desktop-windows-amd64*)    keys=("windows/amd64") ;;
    iterion-desktop-windows-arm64*)    keys=("windows/arm64") ;;
    iterion-desktop-linux-amd64*)      keys=("linux/amd64") ;;
    iterion-desktop-linux-arm64*)      keys=("linux/arm64") ;;
  esac
  if [ "${#keys[@]}" -eq 0 ]; then
    continue
  fi
  size=$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f")
  sha=$(sha256sum "$f" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$f" | awk '{print $1}')
  sigfile="$f.sig"
  if [ ! -f "$sigfile" ]; then
    echo "Missing required signature for $f (expected $sigfile)" >&2
    exit 1
  fi
  sig=$(cat "$sigfile")
  if [ -z "$sig" ]; then
    echo "Empty signature file for $f ($sigfile)" >&2
    exit 1
  fi
  for key in "${keys[@]}"; do
    artifacts[$key]=$(
      printf '"%s":{"url":"%s/%s","size":%s,"sha256":"%s","ed25519":"%s"}' \
        "$key" "$RELEASE_BASE_URL" "$base" "$size" "$sha" "$sig"
    )
  done
done

if [ "${#artifacts[@]}" -eq 0 ]; then
  echo "No signed desktop artifacts found in $DIR" >&2
  exit 1
fi

released_at=$(date -u +%FT%TZ)

{
  printf '{'
  printf '"version":"%s",' "$VERSION"
  printf '"released_at":"%s",' "$released_at"
  printf '"channel":"stable",'
  printf '"artifacts":{'
  first=1
  for key in "${!artifacts[@]}"; do
    [ $first -eq 1 ] || printf ','
    printf '%s' "${artifacts[$key]}"
    first=0
  done
  printf '},'
  printf '"release_notes_url":"https://github.com/SocialGouv/iterion/releases/tag/%s"' "$VERSION"
  printf '}'
} > "$MANIFEST"

# Sign the manifest with the same script used for artefacts.
"$(dirname "$0")"/sign-release.sh "$MANIFEST"

echo "Wrote $MANIFEST"
