#!/usr/bin/env bash
#
# nix-pkgconfig-env.sh — run a command with the GTK3 + WebKitGTK dev headers
# provided by NIX instead of the host package manager, for building the Wails
# desktop app WITHOUT apt/sudo.
#
# Why this exists: `devbox install` links only the RUNTIME output of gtk3 /
# webkitgtk, so the headers + `.pc` files (their `-dev` outputs) are absent and
# `go build -tags desktop,webkit2_41` fails at pkg-config. This script realises
# those `-dev` outputs from the SAME nixpkgs the devbox flake pins (fetched from
# cache.nixos.org — no local compilation), assembles a PKG_CONFIG_PATH from
# their closure, and exports it under the target-specific variable name that the
# nix pkg-config WRAPPER actually reads (it overrides a bare PKG_CONFIG_PATH,
# which is the classic trap). Then it execs the given command.
#
# Usage (run from the repo root — nix must be on PATH; the command runs inside
# devbox so Go/Task are available):
#   scripts/desktop/nix-pkgconfig-env.sh go build -tags desktop,webkit2_41 ./cmd/iterion-desktop/
#   scripts/desktop/nix-pkgconfig-env.sh go test  -tags desktop,webkit2_41 ./cmd/iterion-desktop/
#   scripts/desktop/nix-pkgconfig-env.sh task desktop:build
#
# It reads .devbox/gen/flake/flake.lock for the nixpkgs pin and is idempotent:
# once the dev outputs are in the nix store, re-runs are fast.
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
lock="$repo_root/.devbox/gen/flake/flake.lock"
if [ ! -f "$lock" ]; then
  echo "error: $lock not found — run 'devbox install' first" >&2
  exit 1
fi

# Pinned nixpkgs revision (same one devbox realised the runtime outputs from).
rev=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
for node in d["nodes"].values():
    locked = node.get("locked", {})
    if locked.get("type") == "github" and "nixpkgs" in locked.get("repo", "").lower():
        print(locked["rev"]); break
' "$lock")
if [ -z "${rev:-}" ]; then
  echo "error: could not read nixpkgs rev from $lock" >&2
  exit 1
fi

nixflags=(--extra-experimental-features nix-command --extra-experimental-features flakes)

echo "desktop deps: realising gtk3.dev + webkitgtk_4_1.dev from nixpkgs/$rev (cache.nixos.org)…" >&2
mapfile -t devpaths < <(nix build "${nixflags[@]}" \
  "github:NixOS/nixpkgs/${rev}#gtk3.dev" \
  "github:NixOS/nixpkgs/${rev}#webkitgtk_4_1.dev" \
  --no-link --print-out-paths)

if [ "${#devpaths[@]}" -eq 0 ]; then
  echo "error: nix build produced no output paths" >&2
  exit 1
fi

# Collect every pkgconfig dir in the closure of those dev outputs — this is the
# consistent, version-matched set gtk/webkit's transitive Requires resolve to.
pcp=$(nix-store -qR "${devpaths[@]}" | while read -r p; do
  for d in "$p/lib/pkgconfig" "$p/share/pkgconfig"; do
    # `if/fi`, not `[ -d ] && …`: a trailing failed test would make the loop
    # exit non-zero and, under `set -o pipefail`, kill the whole assignment.
    if [ -d "$d" ]; then printf '%s\n' "$d"; fi
  done
done | sort -u | paste -sd:)

# The nix pkg-config wrapper reads a TARGET-SPECIFIC variable and overwrites a
# bare PKG_CONFIG_PATH, so we must set the target one. Derive the triple from
# the host arch (glibc/linux); set the plain var too as a harmless fallback.
arch=$(uname -m)
target_var="PKG_CONFIG_PATH_${arch}_unknown_linux_gnu"

# Run the command INSIDE devbox (Go/Task live there) with the pkg-config
# variables injected via `env` — devbox run starts a clean environment, so
# exporting them in this outer shell would not survive the boundary.
exec devbox run -- env \
  "${target_var}=${pcp}" \
  "PKG_CONFIG_PATH=${pcp}" \
  "$@"
