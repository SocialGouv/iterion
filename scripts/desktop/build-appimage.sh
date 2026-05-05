#!/usr/bin/env bash
#
# Package a Linux build of iterion-desktop as an AppImage.
#
# Usage:
#   ./scripts/desktop/build-appimage.sh <amd64|arm64>
#
# Pre-requisites (managed by CI; locally install with `apt`):
#   - linuxdeploy / linuxdeploy-plugin-gtk
#   - appimagetool
#   - libfuse2 (for mounting / running the resulting AppImage)
#
# The build/bin/iterion-desktop binary is expected to exist (run
# `task desktop:build:linux:<arch>` first).

set -euo pipefail

ARCH="${1:-amd64}"
APPDIR="build/bin/Iterion.AppDir"
BIN="build/bin/iterion-desktop"

case "$ARCH" in
  amd64) APPIMAGE_ARCH="x86_64" ;;
  arm64) APPIMAGE_ARCH="aarch64" ;;
  *) echo "Unknown arch: $ARCH" >&2; exit 1 ;;
esac

if [ ! -x "$BIN" ]; then
  echo "Binary not found at $BIN — run 'task desktop:build:linux:$ARCH' first" >&2
  exit 1
fi

rm -rf "$APPDIR"
mkdir -p "$APPDIR/usr/bin" "$APPDIR/usr/share/applications" "$APPDIR/usr/share/icons/hicolor/256x256/apps"

cp "$BIN" "$APPDIR/usr/bin/iterion-desktop"
cp build/appicon.png "$APPDIR/usr/share/icons/hicolor/256x256/apps/iterion-desktop.png" 2>/dev/null || true
cp build/appicon.png "$APPDIR/iterion-desktop.png" 2>/dev/null || true
cp build/linux/iterion.desktop "$APPDIR/usr/share/applications/iterion-desktop.desktop"
cp build/linux/iterion.desktop "$APPDIR/iterion-desktop.desktop"
cp build/linux/AppImage/AppRun "$APPDIR/AppRun"
chmod +x "$APPDIR/AppRun"

# Bundle WebKitGTK + dependencies via linuxdeploy-plugin-gtk if available.
if command -v linuxdeploy >/dev/null 2>&1; then
  export ARCH="$APPIMAGE_ARCH"
  linuxdeploy --appdir "$APPDIR" --plugin gtk --output appimage
  mv "Iterion-${APPIMAGE_ARCH}.AppImage" "iterion-desktop-linux-${ARCH}.AppImage" 2>/dev/null || true
elif command -v appimagetool >/dev/null 2>&1; then
  ARCH="$APPIMAGE_ARCH" appimagetool "$APPDIR" "iterion-desktop-linux-${ARCH}.AppImage"
else
  echo "Neither linuxdeploy nor appimagetool found in PATH — install one to package the AppImage" >&2
  exit 1
fi

echo "Built iterion-desktop-linux-${ARCH}.AppImage"
