# Building Iterion Desktop locally

## Prerequisites

1. **devbox** + **direnv** as for the rest of the project (see top-level
   `CLAUDE.md`). The `devbox.json` in this branch pulls in `gtk3`,
   `webkitgtk_4_1`, and `pkg-config` for the Linux Wails build chain.

2. **Wails CLI** — install once into your `$GOPATH/bin`:

   ```bash
   devbox run -- task desktop:install-tools
   ```

   This runs `go install github.com/wailsapp/wails/v2/cmd/wails@latest`.

3. **Frontend deps**: `task editor:build` produces `pkg/server/static/` —
   the desktop binary embeds those exactly like the CLI does. Run it once
   before `task desktop:build`:

   ```bash
   devbox run -- task editor:build
   ```

## Dev mode (hot-reload)

```bash
devbox run -- task desktop:dev
```

Wails opens a window with DevTools enabled (release builds strip them).
Edit a `.tsx`/`.go` file → Wails rebuilds and reloads.

## Production build

```bash
devbox run -- task desktop:build
```

Outputs into `build/bin/`. On macOS that's `Iterion.app`; Linux gives
`iterion-desktop` (use `task desktop:package:linux:amd64` to wrap as an
AppImage); Windows gives `iterion-desktop.exe` plus an NSIS installer.

## First-launch warnings (unsigned binaries)

V1 ships **unsigned**. The first launch on each platform triggers an OS
warning the user must explicitly bypass:

- **macOS Gatekeeper**: right-click the `.app` → Open → Open. After the
  first whitelist Gatekeeper remembers it.
- **Windows SmartScreen**: "Microsoft Defender SmartScreen prevented an
  unrecognized app from starting." Click "More info" → "Run anyway".
- **Linux AppImage**: just `chmod +x` the file. There is no equivalent
  warning.

## Cross-compiling

Wails supports cross-compile targets out of the box. The Taskfile exposes:

| Task | Effect |
|---|---|
| `task desktop:build:darwin:arm64` | Builds for Apple Silicon (must run on macOS) |
| `task desktop:build:darwin:amd64` | Builds for Intel macs (must run on macOS) |
| `task desktop:build:windows:amd64` | Cross or native (NSIS installer included) |
| `task desktop:build:windows:arm64` | Cross or native (NSIS installer included) |
| `task desktop:build:linux:amd64` | Linux x86_64 |
| `task desktop:build:linux:arm64` | Linux aarch64 |

CI builds every target on a matching native runner — see
`.github/workflows/desktop-release.yml`.

## Auto-update key (one-time setup)

Before producing a binary intended for distribution, generate the Ed25519
signing keypair:

```bash
./scripts/desktop/ed25519-keygen.sh
```

Then paste the printed public-key hex into the `updaterPublicKeyHex`
constant in `cmd/iterion-desktop/updater.go` and store the contents of
`~/.iterion-keys/updater_ed25519.pem` in the `UPDATER_ED25519_PRIVATE`
GitHub secret. The script is idempotent — re-running it on a host that
already has the keypair just re-prints the public hex.
