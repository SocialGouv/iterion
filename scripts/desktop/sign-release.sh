#!/usr/bin/env bash
#
# Sign a single release artefact (or every artefact matching a glob) with
# the Ed25519 private key held in the UPDATER_ED25519_PRIVATE env var
# (PEM-encoded). Each input <file> gets a sibling <file>.sig containing
# the hex-encoded signature.
#
# Usage:
#   UPDATER_ED25519_PRIVATE=$(cat ~/.iterion-keys/updater_ed25519.pem) \
#     ./scripts/desktop/sign-release.sh artefacts/iterion-desktop-*
#
# In CI the private key comes from a GitHub secret of the same name.

set -euo pipefail

if [ -z "${UPDATER_ED25519_PRIVATE:-}" ]; then
  echo "UPDATER_ED25519_PRIVATE not set" >&2
  exit 1
fi

KEYFILE=$(mktemp)
trap 'rm -f "$KEYFILE"' EXIT
printf '%s' "$UPDATER_ED25519_PRIVATE" > "$KEYFILE"
chmod 600 "$KEYFILE"

if [ "$#" -eq 0 ]; then
  echo "Usage: $0 <file...>" >&2
  exit 1
fi

# `openssl pkeyutl -rawin` is an OpenSSL ≥ 3.0 extension required for
# Ed25519's "sign the message bytes directly" semantics. macOS ships
# LibreSSL at /usr/bin/openssl, which lacks that flag — so on mac runners
# we route through Homebrew's openssl@3 (preinstalled on the standard
# GitHub-hosted images). Linux/Windows have OpenSSL 3 on $PATH already.
OPENSSL=openssl
for cand in /opt/homebrew/opt/openssl@3/bin/openssl \
            /usr/local/opt/openssl@3/bin/openssl; do
  if [ -x "$cand" ]; then
    OPENSSL="$cand"
    break
  fi
done

for f in "$@"; do
  [ -f "$f" ] || continue
  case "$f" in
    *.sig) continue ;;
  esac
  sig="${f}.sig"
  "$OPENSSL" pkeyutl -sign -inkey "$KEYFILE" -rawin -in "$f" \
    | xxd -p -c 0 \
    > "$sig"
  echo "Signed $f -> $sig"
done
