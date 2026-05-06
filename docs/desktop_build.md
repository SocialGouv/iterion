# Building the Iterion Desktop app

Iterion ships an optional desktop wrapper built with [Wails v2](https://wails.io/).
The desktop binary embeds the editor SPA and the Iterion HTTP server inside a
native window via WebKit (Linux/macOS) or WebView2 (Windows).

This document captures the build pipeline for **all four supported platforms**
and the rationale behind the structural choices that aren't obvious from the
sources alone.

## Architecture rappel

- Sources Go: [cmd/iterion-desktop/](../cmd/iterion-desktop/) — gated by
  `//go:build desktop`. The default `go test ./...` does not require Wails.
- Wails config: [cmd/iterion-desktop/wails.json](../cmd/iterion-desktop/wails.json)
  (and **not** at the repo root — see "wails.json location" below).
- AssetServer: `Assets = nil`, all requests go through a reverse-proxy
  ([asset_proxy.go](../cmd/iterion-desktop/asset_proxy.go)) to the embedded
  `pkg/server` HTTP API. The editor SPA is served from `pkg/server/static`
  via `//go:embed all:static` — never copied into the Wails frontend dir.
- Build tasks: [Taskfile.yml § Desktop](../Taskfile.yml). One target per
  GOOS/GOARCH plus `desktop:package:linux:<arch>` for AppImage.

## wails.json location & `-skipbindings`

Wails CLI insists on running `go build` from the directory holding `wails.json`,
and it generates JS bindings against the `main` package in that directory. Since
our Go main lives in `cmd/iterion-desktop/`, the file is co-located there. The
Taskfile uses a per-task `dir: cmd/iterion-desktop` so the working directory is
always correct.

We pass two Wails flags that aren't part of the default scaffold:

- `-skipbindings`: the editor consumes bindings via the `window.go.main.App.*`
  globals injected by the Wails runtime, not via the static JS shims Wails
  generates. Skipping bindings generation removes a redundant codegen step.
- `-s` (`--skip-frontend`): there is no frontend to build under
  `cmd/iterion-desktop/`. The SPA is built by `task editor:build` and embedded
  into `pkg/server/static`. The proxy in [asset_proxy.go](../cmd/iterion-desktop/asset_proxy.go)
  forwards every request — including the index — to that embedded server.

`-tags desktop,webkit2_41` is required on Linux to opt into the modern WebKit2
GTK 4.1 ABI. Without `webkit2_41` Wails would target webkit2gtk-4.0, which
Debian trixie / Ubuntu 24.04 no longer ship.

## System dependencies

Wails build = native compile = system dev headers. Devbox/Nix is **not** the
right place for them: nixpkgs ships webkitgtk and gtk3 with split outputs, but
devbox only pulls the runtime output, leaving headers and `.pc` files behind.
We use the host package manager instead.

### Debian / Ubuntu (devcontainer + GitHub Actions runners)

```
sudo apt-get install -y \
  libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev \
  fuse libfuse2t64 dpkg-dev patchelf desktop-file-utils \
  gtk-update-icon-cache librsvg2-common xvfb
```

These packages are wired into [.devcontainer/devcontainer.json](../.devcontainer/devcontainer.json)'s
`postCreateCommand`, and into the `Linux deps` step of
[.github/workflows/desktop-release.yml](../.github/workflows/desktop-release.yml).

### macOS

Xcode Command Line Tools provide everything Wails needs (WebKit lives in the
SDK, no extra brew formulas required). The CI runs `macos-13`/`macos-14`.

### Windows

The CI runs on `windows-latest`. Wails uses WebView2 which is bundled with
modern Windows; the build needs `nsis` only for the installer step (`-nsis`
flag in the Taskfile target). On the runner this is auto-installed via the
[`Wails CLI`] download.

## Building locally (Linux)

The codebase is wired for both `devbox run --` workflows and host-native
workflows. Devbox sets up Go + Node + Task; apt provides webkit/gtk dev
headers. The Wails CLI is installed on demand into `$GOPATH/bin`.

```bash
# 1. Install Wails CLI (one-off, per dev environment)
devbox run -- task desktop:install-tools

# 2. Build editor SPA + desktop binary
export PATH="$(go env GOPATH)/bin:$HOME/.local/bin:$PATH"
devbox run -- task desktop:build:linux:amd64
# → build/bin/iterion-desktop-linux-amd64  (~46 MB)

# 3. Optional: smoke-test under Xvfb
xvfb-run -a -s '-screen 0 1280x800x24' \
  timeout 8s build/bin/iterion-desktop-linux-amd64
# Expected: "Editor server listening on http://localhost:<port>"
```

### Packaging as AppImage

The AppImage build needs the AppImage tooling (~20 MB):

```bash
mkdir -p "$HOME/.local/bin"
curl -fsSL -o "$HOME/.local/bin/linuxdeploy" \
  https://github.com/linuxdeploy/linuxdeploy/releases/download/continuous/linuxdeploy-x86_64.AppImage
curl -fsSL -o "$HOME/.local/bin/linuxdeploy-plugin-gtk" \
  https://raw.githubusercontent.com/linuxdeploy/linuxdeploy-plugin-gtk/master/linuxdeploy-plugin-gtk.sh
chmod +x "$HOME/.local/bin/"linuxdeploy*
export PATH="$HOME/.local/bin:$PATH"

devbox run -- task desktop:package:linux:amd64
# → ./iterion-desktop-linux-amd64.AppImage  (~110 MB, includes WebKit)
```

The script ([scripts/desktop/build-appimage.sh](../scripts/desktop/build-appimage.sh))
runs linuxdeploy with `--appimage-extract-and-run`, so it works inside
containers and CI runners that lack `/dev/fuse`.

### Caveat: pkg-config inside devbox shell

devbox's Nix-wrapped pkg-config rewrites `PKG_CONFIG_PATH` to its own Nix store
(which has no webkitgtk dev outputs). When invoking Wails directly inside
`devbox run --`, prepend the system pkg-config to PATH so the wrapper is
bypassed:

```bash
export PATH="/usr/bin:$PATH"
export PKG_CONFIG=/usr/bin/pkg-config
export PKG_CONFIG_PATH=/usr/lib/x86_64-linux-gnu/pkgconfig:/usr/share/pkgconfig
```

The Taskfile entries already invoke wails through the system PATH; this hint
only matters for ad-hoc `wails build` runs.

## Cross-compiling

Wails relies on platform-specific WebView toolchains and CGO, so cross-builds
generally don't work. Each target must be built on a matching host:

| Target            | Build host       |
|-------------------|------------------|
| `linux/amd64`     | any Linux x86_64 (devcontainer OK) |
| `linux/arm64`     | Linux aarch64 (CI uses `ubuntu-24.04-arm`) |
| `darwin/amd64`    | macOS x86_64 (CI uses `macos-13`) |
| `darwin/arm64`    | macOS arm64 (CI uses `macos-14`) |
| `windows/amd64`   | Windows x64 (CI uses `windows-latest`) |
| `windows/arm64`   | Windows x64 cross to arm64 (Wails handles this) |

## CI flow (`.github/workflows/desktop-release.yml`)

1. `actions/setup-go` + `actions/setup-node`.
2. `go install wails@latest` and `go install task@latest`; both end up in
   `$GOPATH/bin`, which is appended to `GITHUB_PATH`.
3. **Linux only**: `apt-get install` of the headers + AppImage tooling listed
   above; `linuxdeploy` and `linuxdeploy-plugin-gtk` fetched from
   github.com/linuxdeploy.
4. `task editor:build` (single source of truth — same task as local dev).
5. `task desktop:build:<os>:<arch>` per matrix entry.
6. Per-package step (zip / nsis-portable / appimage).
7. `readelf` smoke-check on the AppImage to confirm the runtime header arch.
8. Ed25519 signing + manifest publication via
   [scripts/desktop/sign-release.sh](../scripts/desktop/sign-release.sh) and
   [scripts/desktop/generate-manifest.sh](../scripts/desktop/generate-manifest.sh).

## Smoke checklist before shipping a release

- `task desktop:build:linux:amd64` succeeds locally.
- The produced `build/bin/iterion-desktop-linux-amd64` opens a window, loads the
  editor, can run a `.iter` workflow end-to-end.
- The AppImage produced by `task desktop:package:linux:amd64` runs on a
  freshly-installed Debian or Ubuntu without devbox installed.
- Drop a `v*` tag → `desktop-release.yml` job is green for all six matrix
  entries.
