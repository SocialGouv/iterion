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
BIN="build/bin/iterion-desktop-linux-${ARCH}"

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

# Bundle WebKit2GTK helper processes (WebKitWebProcess, WebKitNetworkProcess,
# WebKitGPUProcess + injected-bundle). linuxdeploy-plugin-gtk only copies
# libwebkit2gtk-4.1.so.0; without these out-of-process helpers WebKit loads
# but renders nothing — the WebView appears as a grey rectangle and no
# JavaScript runs. AppRun sets WEBKIT_EXEC_PATH=$HERE/usr/lib/webkit2gtk-4.1
# to point WebKit at the bundled helpers regardless of the host's webkit
# install path (Debian: /usr/lib/x86_64-linux-gnu/webkit2gtk-4.1/, others
# may differ).
WEBKIT_HELPER_DIR="$APPDIR/usr/lib/webkit2gtk-4.1"
mkdir -p "$WEBKIT_HELPER_DIR"
WEBKIT_HELPER_SRC=""
for candidate in \
    "/usr/lib/x86_64-linux-gnu/webkit2gtk-4.1" \
    "/usr/lib/aarch64-linux-gnu/webkit2gtk-4.1" \
    "/usr/lib/webkit2gtk-4.1" \
    "/usr/libexec/webkit2gtk-4.1"; do
  if [ -d "$candidate" ]; then
    WEBKIT_HELPER_SRC="$candidate"
    break
  fi
done
if [ -z "$WEBKIT_HELPER_SRC" ]; then
  echo "ERROR: webkit2gtk-4.1 helper directory not found on build host." >&2
  echo "       Install libwebkit2gtk-4.1-0 (Debian/Ubuntu) and retry." >&2
  exit 1
fi
cp -a "$WEBKIT_HELPER_SRC"/. "$WEBKIT_HELPER_DIR/"

# Bundle WebKitGTK + dependencies via linuxdeploy-plugin-gtk.
# --appimage-extract-and-run lets the AppImage tools run inside containers
# without /dev/fuse (CI, devcontainers).
#
# We exclude libraries that MUST come from the host because they form
# tightly-coupled pairs (libgcrypt + libgpg-error) or version-sensitive
# pairs (libssl + libcrypto with the host's CA trust). Bundling libgcrypt
# from a newer build host while libgpg-error stays on the user host fails
# at runtime with: "undefined symbol: gpgrt_add_post_log_func".
# linuxdeploy-plugin-gtk doesn't honour --exclude-library on its own
# transitive deps, so we also strip the leftovers from AppDir post-bundle.
if ! command -v linuxdeploy >/dev/null 2>&1; then
  echo "linuxdeploy not found in PATH — install it to package the AppImage" >&2
  exit 1
fi

EXCLUDES=(
  --exclude-library=libgcrypt.so\*
  --exclude-library=libgpg-error.so\*
  --exclude-library=libssl.so\*
  --exclude-library=libcrypto.so\*
)

TARGET_NAME="iterion-desktop-linux-${ARCH}.AppImage"
ARCH="$APPIMAGE_ARCH" linuxdeploy --appimage-extract-and-run \
  --appdir "$APPDIR" --plugin gtk --output appimage "${EXCLUDES[@]}"

# Belt-and-suspenders: linuxdeploy-plugin-gtk re-deploys some host-only
# libs after the main excludelist pass. Strip whatever slipped through
# inside the AppImage by extracting, removing, and repacking.
APPIMAGE_GLOB=(./*-"${APPIMAGE_ARCH}.AppImage")
APPIMAGE_FILE="${APPIMAGE_GLOB[0]}"
if [ ! -f "$APPIMAGE_FILE" ]; then
  echo "linuxdeploy did not produce an AppImage matching *-${APPIMAGE_ARCH}.AppImage" >&2
  exit 1
fi

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
(
  cd "$WORKDIR"
  # AppImages are runtime+squashfs concatenated. --appimage-extract gives
  # us a writable copy without needing fuse.
  "$OLDPWD/$APPIMAGE_FILE" --appimage-extract >/dev/null
  for stem in libgcrypt.so libgpg-error.so libssl.so libcrypto.so; do
    find squashfs-root -name "${stem}*" -delete
  done
)

# Repack via appimagetool (downloaded if missing) since linuxdeploy is a
# bundler, not a packer.
APPIMAGETOOL="$HOME/.cache/iterion-appimagetool-${APPIMAGE_ARCH}.AppImage"
if [ ! -x "$APPIMAGETOOL" ]; then
  mkdir -p "$(dirname "$APPIMAGETOOL")"
  curl -fsSL -o "$APPIMAGETOOL" \
    "https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-${APPIMAGE_ARCH}.AppImage"
  chmod +x "$APPIMAGETOOL"
fi

ARCH="$APPIMAGE_ARCH" "$APPIMAGETOOL" --appimage-extract-and-run \
  "$WORKDIR/squashfs-root" "$TARGET_NAME"

# Drop the original (with bundled host-only libs) so the only remaining
# *-${APPIMAGE_ARCH}.AppImage is our cleaned one.
[ "$APPIMAGE_FILE" != "./$TARGET_NAME" ] && rm -f "$APPIMAGE_FILE"

echo "Built ${TARGET_NAME}"
