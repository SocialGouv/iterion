#!/usr/bin/env bash
# Build container entrypoint: run the build under the host user's UID/GID
# (passed via -e HOST_UID/HOST_GID) so generated artefacts aren't owned
# by root on the host bind-mount. Falls back to root when no UID is set
# (e.g. CI inside the same image without a bind-mount).
set -euo pipefail

if [ -n "${HOST_UID:-}" ] && [ -n "${HOST_GID:-}" ]; then
  groupadd -f -g "$HOST_GID" iterion 2>/dev/null || true
  id -u iterion >/dev/null 2>&1 || useradd -u "$HOST_UID" -g "$HOST_GID" -d /workspace -s /bin/bash iterion
  exec setpriv --reuid="$HOST_UID" --regid="$HOST_GID" --init-groups -- "$@"
fi

exec "$@"
