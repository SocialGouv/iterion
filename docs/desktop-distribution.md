# Iterion Desktop — Distribution

## Release flow

Pushing a `v*` tag triggers two GitHub Actions workflows in parallel:

1. **`release.yml`** (existing) — builds the CLI binaries for 6 platforms
   and attaches them to the GitHub Release.
2. **`desktop-release.yml`** (new) — builds the desktop binaries for the
   same 6 platforms, signs each artefact, generates the manifest, and
   attaches everything to the same GitHub Release.

The two workflows do not collide because every desktop artefact is
prefixed `iterion-desktop-*`.

## Artefacts produced per release

```
iterion-desktop-darwin-arm64.zip          # Iterion.app/, ditto-compressed
iterion-desktop-darwin-arm64.zip.sig
iterion-desktop-darwin-amd64.zip
iterion-desktop-darwin-amd64.zip.sig
iterion-desktop-windows-amd64.exe         # portable
iterion-desktop-windows-amd64.exe.sig
iterion-desktop-windows-amd64-installer.exe
iterion-desktop-windows-amd64-installer.exe.sig
iterion-desktop-windows-arm64.exe
iterion-desktop-windows-arm64.exe.sig
iterion-desktop-windows-arm64-installer.exe
iterion-desktop-windows-arm64-installer.exe.sig
iterion-desktop-linux-amd64.AppImage
iterion-desktop-linux-amd64.AppImage.sig
iterion-desktop-linux-arm64.AppImage
iterion-desktop-linux-arm64.AppImage.sig
iterion-desktop-manifest.json
iterion-desktop-manifest.json.sig
```

## Auto-update flow

1. Running desktop polls `releases/latest/download/iterion-desktop-manifest.json`
   on startup (and every 4h thereafter, while the user has auto-check on).
2. Downloads `iterion-desktop-manifest.json.sig` and verifies it against
   the embedded `updaterPublicKeyHex`.
3. Selects the artefact for `GOOS/GOARCH`, downloads it.
4. Verifies the artefact's `sha256` and `ed25519` against the manifest.
5. Atomic-swaps the binary on disk (per-OS strategies in
   `updater_apply_*.go`).
6. Emits a `update:applied` event so the SPA can prompt for restart.

The user can also trigger updates manually via Settings → Updates → "Check
now".

## Signing setup

### One-time: generate keypair

```bash
./scripts/desktop/ed25519-keygen.sh
```

Outputs hex to stdout, stores PEM in `~/.iterion-keys/updater_ed25519.pem`.

### One-time: configure CI

1. Paste the contents of `~/.iterion-keys/updater_ed25519.pem` into a
   GitHub repository secret named `UPDATER_ED25519_PRIVATE`.
2. Paste the public-key hex (the script's stdout) into
   `cmd/iterion-desktop/updater.go`'s `updaterPublicKeyHex` constant
   and commit.
3. **Verify**: build a binary, run `./scripts/desktop/verify-release.sh`
   on its `.sig` against the same hex. Should print `Signature Verified
   Successfully`.

## Rollback

If a desktop release contains a critical bug:

1. Download the previous `iterion-desktop-manifest.json` from the prior
   tag and re-upload it as the latest tag's manifest. This stops auto-update
   from offering the bad version.
2. Optionally, untag the bad release on GitHub (or mark it as draft) so
   the static `releases/latest/...` URL falls through to the previous one.
3. The CLI release remains independent — `release.yml` artefacts are not
   affected.

## Future: code-signing

V1 ships unsigned. When code-signing is added:

- macOS: developer certificate + notarisation via `xcrun notarytool`. Hook
  point in `desktop-release.yml` is gated on `secrets.MACOS_DEVELOPER_ID_CERT_BASE64`.
- Windows: code-signing certificate via `signtool`. Hook point gated on
  `secrets.WINDOWS_CERT_BASE64`.

Adding either does NOT replace the Ed25519 manifest signature — both
exist independently. The Ed25519 layer is the trust anchor for our update
channel; OS signing is only a UX improvement (Gatekeeper / SmartScreen).
