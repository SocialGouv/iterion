# Iterion Desktop — Architecture

The desktop build is a thin native shell wrapped around the existing
`pkg/server` HTTP server and embedded SPA. It adds multi-project state,
OS-keychain credentials, signed auto-update, and native UX (menus, window
state, single-instance, dialogs).

## Process model

```
Iterion.app  /  iterion-desktop.exe  /  Iterion.AppImage
└─ cmd/iterion-desktop  (Wails v2 host, build tag `desktop`)
   ├─ Wails Window — webview pinned to AssetServer start URL
   │     wails://wails/  (Mac/Linux)  or  http://wails.localhost/  (Win)
   ├─ Wails AssetServer  (asset_proxy.go)
   │     ├─ /wails/runtime.js, /wails/ipc.js  (Wails-injected, gives
   │     │       the SPA window.go.main.App.* and window.runtime.*)
   │     └─ everything else  → reverse-proxy → embedded HTTP server
   ├─ Embedded HTTP server  (cli.RunEditor with Port=-1, NoBrowser=true)
   │     ├─ /api/*            REST   (proxied via AssetServer)
   │     ├─ /api/ws/runs/*    WebSocket runs   (DIRECT — Wails AssetServer
   │     │                                       returns 501 on WS upgrade)
   │     └─ /                 SPA (pkg/server/static, go:embed)
   ├─ Wails Bindings (Go ↔ JS — reachable because SPA loads on
   │     AssetServer origin where /wails/runtime.js was injected)
   │     ListProjects / SwitchProject / AddProject / RemoveProject
   │     GetSecret… / SetSecret / DeleteSecret
   │     DetectExternalCLIs / IsFirstRunPending / MarkFirstRunDone
   │     CheckForUpdate / DownloadAndApplyUpdate
   │     OpenExternal / RevealInFinder
   │     GetServerURL / GetSessionToken (used by SPA WS dialer for
   │     the cross-origin ws://127.0.0.1:<port>/api/ws... handshake)
   ├─ Native menus + window state persistence
   └─ Single-instance lock + IPC for "focus existing"
```

## Key decisions

### Why a real `127.0.0.1:<random>` server PLUS Wails AssetServer reverse-proxy

The naive design — load the SPA directly on `http://127.0.0.1:<port>/` —
fails because Wails injects `/wails/runtime.js` and `/wails/ipc.js` ONLY
into HTML responses served by its AssetServer. A page on a different
origin gets no runtime, so `window.go.main.App.*` is `undefined`,
`isDesktop()` returns `false`, and every desktop view silently un-mounts
into "browser mode". (See [ADR-002](adr/002-desktop-assetserver-proxy.md)
for the full investigation that surfaced this in iteration 10.)

The current design solves this by hosting the editor SPA via the Wails
AssetServer's `Handler` (a `httputil.ReverseProxy` in
`cmd/iterion-desktop/asset_proxy.go`), so the WebView's main origin stays
on the AssetServer URL for the lifetime of the app. Wails injects the
runtime into the proxied HTML; bindings remain reachable; and the SPA
runs on the same origin even after the embedded server rebinds on a
fresh port (project switch).

WebSocket upgrades (`/api/ws`, `/api/ws/runs/*`) **cannot** flow through
the proxy — Wails' AssetServer rejects WS with `501` by design — so the
SPA dials them directly at `ws://127.0.0.1:<port>/api/ws...?t=<token>`.
This is the only surface where the SPA touches the loopback origin
directly; HTTP traffic stays on the proxy. We keep a real loopback
server (rather than running everything inside the AssetServer) because:

- `pkg/server` stays 1:1 between CLI (`iterion editor`) and desktop;
- WebSockets keep working without any AssetServer shim;
- the bind interface is loopback-only, plus a per-launch session token,
  so the surface area is no worse than the CLI path.

### Session token

Random 32-byte hex generated once per launch. The
**HTTP path** uses an `iterion_session` HttpOnly cookie attached by the
AssetServer reverse-proxy on every forwarded request — the SPA never has
to learn or echo the token for proxied calls. The **WS path** uses
`?t=<token>` on the upgrade URL because cookies set on the loopback
domain are not sent cross-origin from the AssetServer's `wails://` /
`wails.localhost` page origin. The session middleware accepts both paths;
the WS query path does NOT issue a cookie (the handshake is short-lived).
CLI mode (`iterion editor`) leaves `SessionToken` empty and the
middleware is not installed — byte-identical behaviour.

### No tray in v1

System-tray icons require platform-specific code Wails v2 doesn't supply
out-of-the-box. Closing the window leaves the app in the dock/taskbar
(macOS standard) or terminates the process (Win/Linux). Tray is on the v2
backlog.

### Auto-update — Ed25519, not OS code-signing

V1 ships unsigned binaries (Gatekeeper / SmartScreen will warn the first
time). The auto-updater ALWAYS verifies an embedded Ed25519 public key
against signed manifests + artefacts, regardless of OS code-signing. This
is the trust anchor for v1 and remains valid when OS signing is added later.

### Build-tag isolation

The Wails-importing files carry `//go:build desktop`. `go test ./...`
exercises the desktop package's testable subset (config, external_cli,
path_fix non-darwin) without requiring the Wails CLI or its CGO deps.
Producing the real binary uses `wails build -tags desktop` (which also
flips the right CGO/cross flags).

## File layout

| Path | Purpose |
|---|---|
| `cmd/iterion-desktop/main.go` | Wails entry (desktop tag) |
| `cmd/iterion-desktop/main_stub.go` | Non-desktop fallback (prints usage) |
| `cmd/iterion-desktop/app.go` | Lifecycle (OnStartup/OnShutdown) |
| `cmd/iterion-desktop/bindings.go` | JS-callable methods |
| `cmd/iterion-desktop/menu.go` | Native menus |
| `cmd/iterion-desktop/window_state.go` | Persisted geometry |
| `cmd/iterion-desktop/single_instance.go` + `_unix.go` + `_windows.go` | Single-instance + IPC |
| `cmd/iterion-desktop/server_host.go` | Wraps `cli.RunEditor` |
| `cmd/iterion-desktop/config.go` | User config (no build tag — testable) |
| `cmd/iterion-desktop/keychain.go` | go-keyring wrapper |
| `cmd/iterion-desktop/external_cli.go` | claude/codex/git detection |
| `cmd/iterion-desktop/path_fix_darwin.go` | macOS PATH fix |
| `cmd/iterion-desktop/path_fix_other.go` | No-op on non-darwin |
| `cmd/iterion-desktop/updater*.go` | Ed25519-signed auto-update |
| `cmd/iterion-desktop/asset_proxy.go` | Wails AssetServer reverse-proxy to the embedded HTTP server (see ADR-002) |
| `editor/src/lib/desktopBridge.ts` | Typed `window.go.main.App.*` wrappers |
| `editor/src/hooks/useDesktop.ts` | React hook for desktop state |
| `editor/src/views/Welcome/` | First-run wizard |
| `editor/src/views/Settings/` | API keys / projects / updates / about |
| `editor/src/views/ProjectSwitcher/` | Cmd+P modal |

## Backwards compatibility

The `iterion` CLI is unchanged. `iterion editor` still listens on 4891
without a session token — the CLI flag flows are untouched. Two binaries
ship from the same module: `iterion` (Cobra CLI) and `iterion-desktop`
(Wails app). Both share `pkg/cli`, `pkg/server`, the SPA, and the run
store on disk.
