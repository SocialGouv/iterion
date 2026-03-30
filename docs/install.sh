#!/bin/sh
# Iterion installer
# Usage: curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
set -eu

REPO="SocialGouv/iterion"
BINARY_NAME="iterion"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

info() { printf '\033[1;34m%s\033[0m\n' "$*"; }
error() { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

detect_platform() {
  OS="$(uname -s)"
  case "$OS" in
    Linux)  OS="linux" ;;
    Darwin) OS="darwin" ;;
    MINGW*|MSYS*|CYGWIN*) OS="windows" ;;
    *) error "unsupported OS: $OS" ;;
  esac

  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64)  ARCH="arm64" ;;
    *) error "unsupported architecture: $ARCH" ;;
  esac

  EXT=""
  if [ "$OS" = "windows" ]; then EXT=".exe"; fi
}

get_latest_version() {
  if command -v curl >/dev/null 2>&1; then
    VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')"
  elif command -v wget >/dev/null 2>&1; then
    VERSION="$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')"
  else
    error "curl or wget is required"
  fi
  [ -n "$VERSION" ] || error "could not determine latest version"
}

download() {
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${FILENAME}"
  CHECKSUM_URL="${URL}.sha256"

  info "downloading ${BINARY_NAME} ${VERSION} (${OS}/${ARCH})..."

  TMPDIR="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR"' EXIT

  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$URL" -o "${TMPDIR}/${FILENAME}"
    curl -fsSL "$CHECKSUM_URL" -o "${TMPDIR}/${FILENAME}.sha256" 2>/dev/null || true
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$URL" -O "${TMPDIR}/${FILENAME}"
    wget -q "$CHECKSUM_URL" -O "${TMPDIR}/${FILENAME}.sha256" 2>/dev/null || true
  fi

  # verify checksum if available
  if [ -s "${TMPDIR}/${FILENAME}.sha256" ]; then
    info "verifying checksum..."
    EXPECTED="$(awk '{print $1}' "${TMPDIR}/${FILENAME}.sha256")"
    if command -v sha256sum >/dev/null 2>&1; then
      ACTUAL="$(sha256sum "${TMPDIR}/${FILENAME}" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      ACTUAL="$(shasum -a 256 "${TMPDIR}/${FILENAME}" | awk '{print $1}')"
    else
      ACTUAL=""
    fi
    if [ -n "$ACTUAL" ] && [ "$ACTUAL" != "$EXPECTED" ]; then
      error "checksum mismatch (expected ${EXPECTED}, got ${ACTUAL})"
    fi
  fi
}

install() {
  if [ -w "$INSTALL_DIR" ]; then
    mv "${TMPDIR}/${FILENAME}" "${INSTALL_DIR}/${BINARY_NAME}${EXT}"
    chmod +x "${INSTALL_DIR}/${BINARY_NAME}${EXT}"
  else
    info "installing to ${INSTALL_DIR} (requires sudo)..."
    sudo mv "${TMPDIR}/${FILENAME}" "${INSTALL_DIR}/${BINARY_NAME}${EXT}"
    sudo chmod +x "${INSTALL_DIR}/${BINARY_NAME}${EXT}"
  fi
}

main() {
  detect_platform

  FILENAME="${BINARY_NAME}-${OS}-${ARCH}${EXT}"

  if [ -n "${1:-}" ]; then
    VERSION="$1"
  else
    get_latest_version
  fi

  download
  install

  info "${BINARY_NAME} ${VERSION} installed to ${INSTALL_DIR}/${BINARY_NAME}${EXT}"
  info ""
  info "get started:"
  info "  iterion validate examples/pr_refine_single_model.iter"
}

main "$@"
