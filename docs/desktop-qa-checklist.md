[← docs index](README.md) · [← desktop-qa.md](desktop-qa.md)

# Desktop — release QA matrix

This is the **per-platform sign-off sheet** the team walks before tagging a desktop release. It complements [desktop-qa.md](desktop-qa.md) (the developer-facing smoke checklist focused on AssetServer / runtime-injection regressions) by adding the user-facing scenarios and the assignment grid that says *who* tested *what* on *which platform*.

A release is QA-clean when every box below is ticked and every assignment row is signed off by a human.

## Platforms in scope

| Platform | Binary | Distribution | Owner |
|---|---|---|---|
| **macOS arm64** (Apple Silicon, 14+) | `iterion-desktop-darwin-universal.zip` (lipo'd) | GitHub Releases + brew cask | |
| **macOS amd64** (Intel, 14+) | same universal binary | GitHub Releases + brew cask | |
| **Windows 11** (64-bit) | `iterion-desktop-windows-amd64.exe` + `*-installer.exe` (NSIS) | GitHub Releases | |
| **Windows 10** (1809+, 64-bit) | same .exe | GitHub Releases | |
| **Ubuntu 22.04 LTS** | `iterion-desktop-linux-amd64.AppImage` + `*.deb` | GitHub Releases | |
| **Fedora 40+** | `iterion-desktop-linux-amd64.AppImage` (.deb not native; AppImage runs anywhere) | GitHub Releases | |

Multiple binaries (universal vs split, AppImage vs .deb) means the same scenario must be exercised against each delivery for that platform — the bundle, not the OS, is what can break.

## Scenarios

For every platform row, walk every scenario column. Mark `✓` (pass), `✗` (fail with bug ticket #), or `n/a` (not applicable).

### 1. Boot

- [ ] First-launch with no pre-existing config shows the Welcome wizard.
- [ ] Single-instance enforced (launching a second iterion-desktop focuses the existing window, doesn't spawn a duplicate).
- [ ] Window position + size restored from previous session (multi-monitor: re-centred if previous origin is off-screen).
- [ ] Tray / dock icon appears and is interactive (Show/Hide, Quit).
- [ ] `Help → About` shows the correct version + commit SHA + release notes URL.

### 2. Multi-project

- [ ] Open project A → switch to project B via the project switcher (Cmd+P / Ctrl+P).
- [ ] Project switch reloads the SPA, re-binds the WebSocket, invalidates the AssetServer proxy cache (no stale pages).
- [ ] Close project B → reopen → restored cleanly.
- [ ] Close all projects → Welcome wizard appears (or stays visible).

### 3. Settings

- [ ] Open Settings; theme toggle persists across restart.
- [ ] API key entry → save → reopen Settings → key is masked but present.
- [ ] OS keychain integration: `security find-generic-password -s iterion-desktop` (macOS) / Credential Manager (Windows) / libsecret (Linux) shows the entry; deleting it from keychain unsets it in-app on next read.
- [ ] Telemetry opt-out (if surfaced) takes effect immediately.

### 4. Onboarding (Welcome wizard)

- [ ] CLI detection step finds installed `claude`, `codex`, `claw` binaries when present.
- [ ] Missing-CLI banner appears for absent binaries with a working install link.
- [ ] Wizard completes → main editor view loads with the selected backend pre-selected.

### 5. Run (workflow execution)

- [ ] Open `examples/hello.iter` (or scaffold via `iterion init`).
- [ ] Click **Run** → live console renders events as they stream.
- [ ] Scrubber: drag back and forth across iterations; per-iteration node detail loads.
- [ ] Resume a paused run via the resume button — picks up from the checkpoint without re-running upstream.
- [ ] `worktree: auto` workflow: post-run, verify `final_branch` is created and (when current branch is clean) fast-forwarded; check `git branch -a | grep iterion/run/`.

### 6. Browser pane (PR 1-4)

- [ ] Workflow with a `tool` node emitting `[iterion] preview_url=…` opens the browser pane in passive iframe mode.
- [ ] Screenshots scrubber: drag the timeline; the pane shows the historic screenshot for that seq.
- [ ] Live mode (PR 4): a workflow with `browser_*` Playwright tools drives a host Chromium; the pane shows the live CDP feed.
- [ ] Browser session ends cleanly when the run finishes (no orphan Chromium processes; verify `pgrep -f chromium` after run completion).

### 7. Auto-update

- [ ] Running version `vN-1` (the prior release).
- [ ] Manifest URL responds with current release JSON + `.sig`.
- [ ] App detects the new version on its periodic check (manual trigger via `Help → Check for updates` if surfaced).
- [ ] Signature verification passes (Ed25519 against `updaterPublicKeyHex` baked in).
- [ ] Downloaded artefact SHA256 matches the manifest entry.
- [ ] App applies the update and relaunches.
- [ ] Post-relaunch, window state (size, position, maximised flag, current project) is preserved.
- [ ] Settings (API keys via keychain, theme) preserved.
- [ ] **Tampering check**: serve a manifest with a corrupted signature → app refuses the update (no silent fallback to the bad artefact).
- [ ] **Downgrade refused**: serve a manifest with a version *older* than current → app stays on current version.

### 8. Crash recovery

- [ ] Force-kill the app mid-run (`kill -9` / Task Manager / Force Quit).
- [ ] Relaunch → no corrupted run state; orphan run reconciliation marks the run as `failed_resumable` if it was in flight.
- [ ] Single-instance lockfile: stale lock from previous session is cleaned up on relaunch (no "another instance is already running" false positive).

### 9. Disconnect (network resilience)

- [ ] Start a long-running cloud-mode run.
- [ ] Drop network for 30s (turn off WiFi / use a network namespace / unplug Ethernet).
- [ ] Reconnect → app reconnects to the WebSocket; the event stream catches up.
- [ ] No duplicate events post-reconnect (the editor's event de-dup by seq holds).

### 10. Distribution-specific

**macOS**:
- [ ] `.app` opens via Gatekeeper without the "unidentified developer" warning (post-notarization).
- [ ] `xattr -p com.apple.quarantine /Applications/Iterion.app` → no quarantine flag after first run.
- [ ] Universal binary runs natively on both Intel and Apple Silicon (`file Contents/MacOS/iterion-desktop` → "Mach-O universal binary with 2 architectures").

**Windows**:
- [ ] SmartScreen does not block the .exe after download (post-Authenticode signing + reputation warmup).
- [ ] NSIS installer creates Start Menu shortcut + uninstaller.
- [ ] Uninstall removes the binary + Start Menu entry; user data in `%APPDATA%/iterion-desktop/` is preserved (or removed per uninstaller policy).

**Linux (Ubuntu)**:
- [ ] `chmod +x iterion-desktop-linux-amd64.AppImage && ./iterion-desktop-linux-amd64.AppImage` runs without prompting for fuse.
- [ ] `sudo dpkg -i iterion-desktop_*.deb` installs to `/usr/local/bin`; postinst refreshes desktop-database + icon-cache.
- [ ] `sudo dpkg -r iterion-desktop` removes cleanly; postrm refreshes desktop-database.

**Linux (Fedora)**:
- [ ] AppImage runs (Fedora doesn't ship `libfuse2` by default; `dnf install fuse-libs` may be needed; verify the error path is clear).

## Assignment grid

Fill in tester name + date for each (platform, scenario) cell. A row is complete when every scenario is signed off.

|  | Boot | Multi-proj | Settings | Onboarding | Run | Browser | Auto-update | Crash | Disconnect | Dist-specific |
|---|---|---|---|---|---|---|---|---|---|---|
| macOS arm64 | | | | | | | | | | |
| macOS amd64 | | | | | | | | | | |
| Windows 11 | | | | | | | | | | |
| Windows 10 | | | | | | | | | | |
| Ubuntu 22.04 | | | | | | | | | | |
| Fedora 40+ | | | | | | | | | | |

## Bug filing

Failures get a GitHub issue with the label `desktop-qa` and the failing platform + scenario in the title. Block the release on any P0/P1 (data loss, crash on first launch, auto-update bricks the install). P2 (cosmetic, single-platform niceties) can ship as a known issue with a target patch release.

## What's not here

- Performance benchmarks (cold-start time, memory at idle) — covered by [desktop-qa.md](desktop-qa.md) when it grows.
- Localisation / i18n — out of v1.0 scope.
- Accessibility audit (VoiceOver, NVDA, Orca) — should be added before v1.1.

## After the release

- Update [desktop-distribution.md](desktop-distribution.md) with the new manifest hash and any cert-rotation notes.
- File any deferred bugs as `desktop-v1.1-target` to keep the next release planning visible.
- Capture lessons learned in this file's history — what scenario the matrix missed, what bug snuck through; that's how the matrix improves.
