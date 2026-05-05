# Iterion Desktop — QA Checklist

Walk this list before tagging a release.

## Smoke

- [ ] `task desktop:build` produces a runnable binary on the current OS.
- [ ] First launch (no pre-existing config) shows the Welcome wizard.
      **Critical regression check** — this exercises the whole Wails-IPC path:
      Welcome only renders when `isDesktop && firstRunPending`, both of which
      require the SPA to have access to `window.go.main.App.*`. If you see
      the regular editor home page on a clean config, bindings are gone and
      the AssetServer reverse-proxy / runtime injection is broken (the bug
      [ADR-002](adr/002-desktop-assetserver-proxy.md) was created to fix).
- [ ] Open the WebView's DevTools (View → Toggle DevTools in dev builds)
      and confirm `window.go.main.App` and `window.runtime` are both
      defined. If undefined, the AssetServer reverse-proxy isn't injecting
      the runtime into proxied HTML (regression of ADR-002).
- [ ] Second launch restores the previous project + window geometry.
- [ ] `Help → About` shows the correct version + commit SHA.

## Multi-project

- [ ] Add three projects via the Welcome flow + `+ Add project…` in the
      switcher. All appear in the recent list.
- [ ] Cmd+P opens the switcher, fuzzy match works on name and path.
- [ ] Switching projects reloads the SPA on the right working directory.
- [ ] **Post-onboarding sanity (the embedded HTTP server rebinds on a
      fresh ephemeral port at every restart; the AssetServer reverse-proxy
      must rebind to the new target):** complete the Welcome flow on a
      clean config, then on the editor home page verify that
        * the file list at the left actually populates (no spinner stuck +
          no `ERR_CONNECTION_REFUSED`),
        * a workflow can be opened and the Run console connects its
          `/api/ws/runs/...` WebSocket without errors in DevTools.
      Any of these failing means the WindowReloadApp re-bootstrap regressed
      OR the AssetServer reverse-proxy isn't picking up the new
      `serverURL` (regression of asset_proxy.go cache invalidation).
- [ ] Cmd+P switch between two projects, then on the new project verify
      the same two checks above. The window URL in DevTools should remain
      the AssetServer URL (`wails://wails/` on Mac/Linux, `http://wails.
      localhost/` on Windows) — the proxy rebinds invisibly to the new
      `127.0.0.1:<port>` upstream. The DevTools "Network" tab should show
      `/api/*` requests resolving against the AssetServer origin (forwarded
      to the new local server) and `/api/ws/...` requests dialing
      `ws://127.0.0.1:<new_port>/api/ws/runs/...?t=<token>` directly.
- [ ] Removing a project leaves its filesystem untouched.

## Settings

- [ ] Adding an API key via Settings → API keys → Save persists across
      restart.
- [ ] If a shell env var of the same name is already set when the desktop
      launches, the Settings page shows the "shadowed by env" badge for
      that key.
- [ ] Deleting a key removes the badge and frees the keychain slot.

## Workflows

- [ ] `examples/hello.iter` runs end-to-end with real-time event stream.
- [ ] A workflow that uses `claude_code` either succeeds (CLI installed)
      or fails with a clear error message (CLI missing).
- [ ] Worktree finalisation: a workflow with `worktree: auto` ends with a
      visible new branch on the chosen target.

## Lifecycle

- [ ] Closing the window while a run is in-flight triggers the 60s drain
      and flips the run to `failed_resumable`.
- [ ] `iterion resume --run-id …` (CLI) successfully resumes the run.
- [ ] WebSocket reconnects after macOS clamshell sleep (close lid 30s,
      open) — stream resumes without manual reload.

## Single-instance

- [ ] Launching a second instance from Finder/Dock surfaces the existing
      window and the second process exits silently.
- [ ] If the lockfile is stale (kill -9 the running process), the next
      launch acquires the lock cleanly.

## Auto-update

- [ ] Help → Check for Updates… reports "up to date" when the running
      version equals the manifest's `version`.
- [ ] When pointing `ITERION_UPDATE_MANIFEST_URL` at a local manifest with
      a higher version, the Updates tab proposes the new version.
- [ ] Tampering with the manifest after signing is rejected with a clear
      error.
- [ ] Tampering with the artefact after signing is rejected (sha256 fail).
- [ ] After a successful download + verify, the binary is replaced
      atomically (no half-written `.app`/`.exe`/`.AppImage`).

## Cross-platform

- [ ] Run the smoke checklist on macOS arm64 + amd64.
- [ ] Run on Windows 11 + Windows 10.
- [ ] Run on Ubuntu 22.04 + Fedora latest from the AppImage.

## Release artefacts

- [ ] Tagging `vX.Y.Z` on a release branch produces 6 binaries + manifest +
      every `.sig` in the GitHub Release.
- [ ] Manifest URL `releases/latest/download/iterion-desktop-manifest.json`
      resolves to the new manifest.
