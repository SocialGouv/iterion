#!/usr/bin/env bash
#
# Build a .deb package from already-compiled iterion-desktop and
# iterion CLI binaries. Mirrors the "Bundle Linux .deb package" step
# of the CI workflow (.github/workflows/desktop-release.yml) so local
# iteration doesn't require a tag-and-push round-trip.
#
# Usage:
#   ./scripts/desktop/build-deb.sh <arch>
# where <arch> is one of: amd64, arm64
#
# Requires:
#   - build/bin/iterion-desktop-linux-<arch>      (run task desktop:build:linux:<arch> first)
#   - build/bin/iterion-linux-<arch>              (run task cli:build:linux:<arch> first)
#   - build/linux/iterion.desktop                 (in-tree)
#   - build/appicon.png                           (in-tree)
#   - dpkg-deb on PATH                            (apt install dpkg-dev)
#   - node on PATH                                (devbox provides Node 22)
#
# Output:
#   build/bin/iterion-desktop-linux-<arch>.deb
#
# Install the result with `sudo dpkg -i ...` or `sudo apt install ./...`.
# The package installs:
#   /usr/bin/iterion-desktop  — the GUI
#   /usr/bin/iterion          — the CLI (run, validate, diagram, …)
# both pinned to the same source revision so editor and CLI cannot
# drift between releases.

set -euo pipefail

ARCH="${1:-}"
case "$ARCH" in
  amd64|arm64) ;;
  *)
    echo "usage: $0 <amd64|arm64>" >&2
    exit 2
    ;;
esac

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

ARTIFACT_NAME="iterion-desktop-linux-${ARCH}"
BIN_PATH="build/bin/${ARTIFACT_NAME}"
CLI_BIN_PATH="build/bin/iterion-linux-${ARCH}"

if [ ! -f "$BIN_PATH" ]; then
  echo "ERROR: $BIN_PATH not found." >&2
  echo "Run 'devbox run -- task desktop:build:linux:${ARCH}' first." >&2
  exit 1
fi
if [ ! -f "$CLI_BIN_PATH" ]; then
  echo "ERROR: $CLI_BIN_PATH not found." >&2
  echo "Run 'devbox run -- task cli:build:linux:${ARCH}' first." >&2
  exit 1
fi
if ! command -v dpkg-deb >/dev/null 2>&1; then
  echo "ERROR: dpkg-deb not on PATH (apt install dpkg-dev)." >&2
  exit 1
fi

VERSION="${VERSION:-$(node -p "require('./package.json').version")}"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

mkdir -p \
  "$stage/DEBIAN" \
  "$stage/usr/bin" \
  "$stage/usr/share/applications" \
  "$stage/usr/share/icons/hicolor/256x256/apps" \
  "$stage/usr/share/doc/iterion-desktop"

install -m 755 "$BIN_PATH" "$stage/usr/bin/iterion-desktop"
install -m 755 "$CLI_BIN_PATH" "$stage/usr/bin/iterion"
install -m 644 build/linux/iterion.desktop \
  "$stage/usr/share/applications/iterion-desktop.desktop"
if [ -f build/appicon.png ]; then
  install -m 644 build/appicon.png \
    "$stage/usr/share/icons/hicolor/256x256/apps/iterion-desktop.png"
fi

cat > "$stage/usr/share/doc/iterion-desktop/copyright" <<'EOF'
Iterion Desktop
Upstream: https://github.com/SocialGouv/iterion
License: Apache-2.0
EOF

installed_size="$(du -sk "$stage" | awk '{print $1}')"

cat > "$stage/DEBIAN/control" <<EOF
Package: iterion-desktop
Version: ${VERSION}
Section: devel
Priority: optional
Architecture: ${ARCH}
Depends: libgtk-3-0 | libgtk-3-0t64, libwebkit2gtk-4.1-0, libsoup-3.0-0
Recommends: gtk-update-icon-cache
Maintainer: SocialGouv <opensource@social.gouv.fr>
Homepage: https://github.com/SocialGouv/iterion
Installed-Size: ${installed_size}
Description: Iterion Desktop — workflow orchestration for AI agents
 Native desktop wrapper around the iterion editor and runtime,
 plus the iterion CLI (run, validate, diagram, inspect, resume,
 report, …). Both binaries are built from the same source revision
 so the editor and the CLI cannot drift between releases.
 Runs against the system WebKitGTK 4.1 stack (smaller than the
 AppImage variant).
EOF

cat > "$stage/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if [ -x "$(command -v gtk-update-icon-cache)" ]; then
  gtk-update-icon-cache -q /usr/share/icons/hicolor || true
fi
if [ -x "$(command -v update-desktop-database)" ]; then
  update-desktop-database -q /usr/share/applications || true
fi
EOF
cp "$stage/DEBIAN/postinst" "$stage/DEBIAN/postrm"
chmod 755 "$stage/DEBIAN/postinst" "$stage/DEBIAN/postrm"

OUT="build/bin/${ARTIFACT_NAME}.deb"
dpkg-deb --build --root-owner-group "$stage" "$OUT"

ls -la "$OUT"
echo
echo "Install with:"
echo "  sudo dpkg -i $OUT"
echo "or:"
echo "  sudo apt install ./$OUT"
