#!/usr/bin/env bash
#
# Verify an Ed25519 signature against a public key. Used during local
# debugging — production verification happens inside the desktop binary
# (cmd/iterion-desktop/updater.go).
#
# Usage:
#   ./scripts/desktop/verify-release.sh <pubkey.hex> <file>
#
# Where <pubkey.hex> is the hex-encoded raw public key (32 bytes) — the
# same value embedded in updaterPublicKeyHex.

set -euo pipefail

PUBKEY_HEX="${1:?usage: verify-release.sh <pubkey.hex> <file>}"
FILE="${2:?usage: verify-release.sh <pubkey.hex> <file>}"
SIG="${FILE}.sig"

if [ ! -f "$SIG" ]; then
  echo "Missing signature: $SIG" >&2
  exit 1
fi

# Decode pubkey hex into raw bytes, prepend the standard Ed25519
# subjectPublicKeyInfo prefix, write a PEM, then verify.
PEM=$(mktemp)
trap 'rm -f "$PEM" "$BIN_PUBKEY" "$BIN_SIG"' EXIT

BIN_PUBKEY=$(mktemp)
printf '%s' "$PUBKEY_HEX" | xxd -r -p > "$BIN_PUBKEY"

# Wrap raw 32-byte ed25519 pubkey in the SubjectPublicKeyInfo ASN.1
# wrapper expected by openssl.
{
  printf '\x30\x2a\x30\x05\x06\x03\x2b\x65\x70\x03\x21\x00'
  cat "$BIN_PUBKEY"
} | openssl pkey -inform DER -pubin -out "$PEM"

BIN_SIG=$(mktemp)
xxd -r -p < "$SIG" > "$BIN_SIG"

openssl pkeyutl -verify -pubin -inkey "$PEM" -rawin -in "$FILE" -sigfile "$BIN_SIG"
