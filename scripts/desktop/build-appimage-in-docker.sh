#!/usr/bin/env bash
# Build an Iterion Desktop Linux AppImage inside a pinned Ubuntu 22.04
# container, so the resulting binary is portable across all major
# desktop distros from Ubuntu 22.04 (glibc 2.35) onward.
#
# Usage:
#   scripts/desktop/build-appimage-in-docker.sh [amd64|arm64]
#
# Requirements on the host:
#   - docker (working daemon)
#   - the repository checked out at $PWD or invoked from any subdir
#
# The container mounts the repo read-write (so build/ and the AppImage
# get written back), forces the host UID/GID through entrypoint.sh, and
# runs `task desktop:package:linux:<arch>` inside.

set -euo pipefail

ARCH="${1:-amd64}"
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unknown arch: $ARCH (expected amd64|arm64)" >&2; exit 1 ;;
esac

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
DOCKERFILE="$REPO_ROOT/scripts/desktop/Dockerfile.appimage"
IMAGE_TAG="iterion-appimage-builder:ubuntu22.04-${ARCH}"
PLATFORM="linux/${ARCH}"

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker is required (install + start the daemon, or rebuild" >&2
  echo "       the devcontainer with the docker-in-docker feature enabled)." >&2
  exit 1
fi

echo "▶ Building image $IMAGE_TAG (platform=$PLATFORM)…"
docker build \
  --platform="$PLATFORM" \
  -f "$DOCKERFILE" \
  -t "$IMAGE_TAG" \
  "$(dirname "$DOCKERFILE")"

echo "▶ Running build inside container…"
docker run --rm \
  --platform="$PLATFORM" \
  -v "$REPO_ROOT:/workspace" \
  -e HOST_UID="$(id -u)" \
  -e HOST_GID="$(id -g)" \
  "$IMAGE_TAG" \
  task "desktop:package:linux:${ARCH}"

echo "✔ AppImage produced at: $REPO_ROOT/iterion-desktop-linux-${ARCH}.AppImage"
