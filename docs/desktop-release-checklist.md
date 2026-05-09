[← docs index](README.md) · [← desktop-qa-checklist.md](desktop-qa-checklist.md) · [← desktop-distribution.md](desktop-distribution.md)

# Desktop — release checklist

Walk this checklist before pushing a `desktop-v*` (or `v*` if CLI + desktop release together) tag. It's the gate between "code is QA-clean on every platform" (covered by [desktop-qa-checklist.md](desktop-qa-checklist.md)) and "the release is published, signed, and announced".

A box left unticked is a release that ships broken. Resist the urge to bypass.

## 1. Code freeze

- [ ] All changes intended for the release are merged to `main`.
- [ ] CI is green on the head commit (lint, unit tests, e2e tests, Trivy).
- [ ] [docs/desktop-qa-checklist.md](desktop-qa-checklist.md) — assignment grid filled, every cell signed off.
- [ ] No P0 or P1 bugs labelled `desktop-qa` open against the release branch.

## 2. Versioning

- [ ] `package.json` version bumped (or `release-it` ran successfully and bumped it).
- [ ] `charts/iterion/Chart.yaml` `appVersion` matches `package.json` (CI guard would block otherwise; verify locally with `task chart:sync-version`).
- [ ] `CHANGELOG.md` updated (release-it generates from conventional commits; spot-check for missing entries).

## 3. Signing prerequisites

These are **hard prerequisites** held outside the repo. Confirm each is current and accessible by the release operator before pushing the tag.

- [ ] **Ed25519 updater key**: GitHub secret `UPDATER_ED25519_PRIVATE` is set, in PEM form, matches the public key embedded in `cmd/iterion-desktop/updater.go` (`updaterPublicKeyHex` constant). If the constant has changed since the last release, see "Key rotation" below.
- [ ] **macOS Apple Developer ID**: `APPLE_DEVELOPER_ID_CERT` (.p12) and `APPLE_DEVELOPER_ID_CERT_PASSWORD` GitHub secrets present; `APPLE_NOTARIZE_API_KEY` (.p8) + `APPLE_NOTARIZE_KEY_ID` + `APPLE_NOTARIZE_ISSUER_ID` present. Cert validity > release date + 30 days.
- [ ] **Microsoft Authenticode**: `WINDOWS_SIGNING_CERT` (.pfx) and `WINDOWS_SIGNING_CERT_PASSWORD` (or SignPath / Azure Trusted Signing config) present. Cert validity > release date + 30 days.
- [ ] **GPG signing key for Linux**: `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE` GitHub secrets present (for signing `.deb` + `.AppImage`). Public key published to the org's key server / `KEYS` file in the release.

If any of these is missing or expired:
- For Ed25519: see "Key rotation".
- For Apple / Microsoft / GPG: pause the release until the operator with cert custody has refreshed the secret. Skipping signing on a public release is grounds to *not* ship.

## 4. Release-pipeline dry run (optional but recommended)

- [ ] On a feature branch, run `gh workflow run desktop-release.yml -f dry_run=true` (if the workflow supports it) or use a `desktop-vX.Y.Z-rc1` pre-release tag.
- [ ] Verify all 6 platform jobs succeed (macOS universal, windows/amd64, windows/arm64, linux/amd64, linux/arm64).
- [ ] Verify `generate-manifest.sh` produced valid JSON (`jq -e .` against the artefact).
- [ ] Verify the generated `.sig` files are valid Ed25519 signatures of their corresponding artefacts (sample: download one binary + its `.sig`, run `openssl pkeyutl -verify -pubin -inkey updater_ed25519.pub -rawin -in <artefact> -sigfile <artefact>.sig`).

## 5. Trigger the release

- [ ] Tag and push: `git tag v<X.Y.Z> && git push origin v<X.Y.Z>` (or use the `release-it` flow).
- [ ] Watch `desktop-release.yml` succeed end-to-end.
- [ ] Watch `release.yml` (CLI) succeed end-to-end (parallel job on the same tag).
- [ ] Watch `brew-update.yml` (`workflow_run` on the above) succeed and patch the brew tap's cask SHA.

## 6. Post-publish verification

- [ ] `gh release view v<X.Y.Z>` shows all expected artefacts (6 binaries × {bundle, .sig} + manifest + manifest.sig + checksum + GPG sigs for Linux).
- [ ] Download and run a binary on each platform that wasn't covered by CI:
  - Open the .zip on a real Apple Silicon Mac → `/Applications/Iterion.app` opens cleanly via Gatekeeper.
  - Run the .exe-installer on a Windows host → SmartScreen accepts (post Authenticode warmup).
  - `chmod +x` and run the AppImage on Ubuntu → opens.
- [ ] Auto-update test: install vN-1 on a real machine, run `Help → Check for updates`, watch the upgrade complete.
- [ ] Brew tap: `brew update && brew install iterion-desktop` (or `brew upgrade iterion-desktop`) → installs vN.

## 7. Announcement (optional but expected for v1.0)

- [ ] Release notes drafted from `CHANGELOG.md` and the manifesto's "what's new" framing.
- [ ] GitHub Release body populated with the announcement.
- [ ] Blog post / changelog entry / tweet drafted.
- [ ] Update README.md "latest release" badges (if any).
- [ ] Update [why-iterion.md](why-iterion.md) "How to start" if the install URL or scaffolding differs.

## 8. Rollback contingency

If a release artefact ships a critical bug after publication:

- [ ] **Yank the manifest**: edit the GitHub Release to remove or replace `iterion-desktop-manifest.json` so existing users don't auto-update to the broken version. New users land on a stale version, which is preferred.
- [ ] **Re-tag a `vX.Y.Z+1` patch release** with the fix and publish; auto-update will lift users off the broken version.
- [ ] **Revert the brew cask** to the previous good version manually (the cask edits live in the iterion-brew tap repo).
- [ ] **Communicate**: pin a notice on the GitHub Release describing the issue, recommended action, and patch ETA.
- [ ] **Post-mortem**: track the regression that escaped CI/QA; add a scenario to [desktop-qa-checklist.md](desktop-qa-checklist.md) so it can't recur.

## Key rotation (Ed25519 updater key)

The Ed25519 keypair signing the manifest + artefacts is the trust root for auto-update. Rotate it when:

- The private key is exposed (anywhere outside the GitHub secret store).
- The release operator with custody changes (transfer is itself an exposure window).
- It's been > 2 years since last rotation (defence-in-depth; algorithm is fine, custody isn't).

Rotation procedure:

1. Generate a new keypair: `./scripts/desktop/ed25519-keygen.sh ./new-keys`.
2. Update `cmd/iterion-desktop/updater.go` `updaterPublicKeyHex` to the new public key (hex-encoded).
3. Cut a release with **both** the old and new public keys recognised — temporarily widen `verifyManifest` to accept either signature, ship that release, wait until > 95% of installs have updated.
4. Cut a follow-up release that drops the old public key.
5. Update the GitHub secret `UPDATER_ED25519_PRIVATE` to the new key.
6. Securely destroy the old private key.

Without the dual-key bridge, users on the old version can't update past the rotation point because their embedded public key won't verify the new manifest.

## What's not here

- CLI-only release process — covered by `release.yml` and the conventional-commits / release-it flow. The desktop release rides the same tag.
- Detailed cert provisioning (how to request an Apple Developer ID, etc.) — out of scope; once you have the cert, this checklist tells you how to use it.
- Marketing / press kit — that's a product call, not a release-engineering call.
