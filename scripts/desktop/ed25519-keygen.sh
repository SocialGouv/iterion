#!/usr/bin/env bash
#
# Generate or read an Ed25519 keypair used to sign Iterion Desktop release
# manifests and artefacts.
#
# Idempotent: if ~/.iterion-keys/updater_ed25519.pem already exists, this
# script does NOT overwrite it. It always prints the public key in hex on
# stdout (suitable for embedding into cmd/iterion-desktop/updater.go's
# updaterPublicKeyHex constant).
#
# Usage:
#   ./scripts/desktop/ed25519-keygen.sh
#
# After generating, copy the public hex into cmd/iterion-desktop/updater.go
# and store the contents of updater_ed25519.pem in the GitHub secret named
# UPDATER_ED25519_PRIVATE.

set -euo pipefail

KEY_DIR="${ITERION_KEY_DIR:-$HOME/.iterion-keys}"
PRIV_PATH="$KEY_DIR/updater_ed25519.pem"
PUB_PATH="$KEY_DIR/updater_ed25519.pub.hex"

mkdir -p "$KEY_DIR"
chmod 700 "$KEY_DIR"

if [ ! -f "$PRIV_PATH" ]; then
  echo "Generating new Ed25519 keypair at $PRIV_PATH" >&2
  openssl genpkey -algorithm ED25519 -out "$PRIV_PATH"
  chmod 600 "$PRIV_PATH"
else
  echo "Found existing private key at $PRIV_PATH (not overwriting)" >&2
fi

# Extract raw public key bytes (last 32 bytes of the DER) and print as hex.
# OpenSSL's `pkey -pubout` gives PEM with a 12-byte ASN.1 prefix on Ed25519;
# the last 32 bytes are the raw key.
PUB_HEX=$(openssl pkey -in "$PRIV_PATH" -pubout -outform DER 2>/dev/null \
  | tail -c 32 \
  | xxd -p -c 64)

if [ -z "$PUB_HEX" ]; then
  echo "ERROR: failed to extract public key" >&2
  exit 1
fi

echo "$PUB_HEX" > "$PUB_PATH"
echo "$PUB_HEX"
