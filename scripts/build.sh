#!/usr/bin/env bash
set -euo pipefail

VERSION="${npm_package_version:-dev}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo '')"
LDFLAGS="-X github.com/SocialGouv/iterion/internal/appinfo.Version=v${VERSION} -X github.com/SocialGouv/iterion/internal/appinfo.Commit=${COMMIT}"

# Build editor frontend (embedded static files)
cd editor
pnpm install --prefer-offline
pnpm exec vite build
rm -rf ../server/static/assets ../server/static/index.html
cp -r dist/* ../server/static/
cd ..

# Build Go binary
CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o iterion ./cmd/iterion
