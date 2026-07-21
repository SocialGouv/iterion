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
   │     ├─ /, /assets/*       SPA/static from the GUI binary's embedded
   │     │                     pkg/server.StaticFS
   │     └─ /api/*             reverse-proxy → selected loopback server
   ├─ Selected loopback server (default: per-project headless daemon;
   │  fallback/opt-out: in-process ServerHost via cli.RunStudio)
   │     ├─ /api/*            REST   (proxied via AssetServer)
   │     └─ /api/ws/runs/*    WebSocket runs   (DIRECT — Wails AssetServer
   │                                             returns 501 on WS upgrade)
   ├─ Wails Bindings (Go ↔ JS — reachable because SPA loads on
   │     AssetServer origin where /wails/runtime.js was injected)
   │     ListProjects / SwitchProject / AddProject / RemoveProject
   │     GetSecret… / SetSecret / DeleteSecret
   │     DetectExternalCLIs / IsFirstRunPending / MarkFirstRunDone
   │     CheckForUpdate / DownloadAndApplyUpdate
   │     OpenExternal / RevealInFinder
   │     GetServerURL / GetSessionToken (SPA WS helper; local desktop
   │     returns an empty token and dials ws://127.0.0.1:<port>/api/ws...)
   ├─ Native menus + window state persistence
   └─ Single-instance lock + IPC for "focus existing"
```

## Multi-connection workspace (local + cloud side by side)

The desktop is a **workspace**, not a single-connection window: several
connections — local projects and remote cloud instances — are open and
LIVE at once, each in its own pane, shown as tabs or split side by side.
Connecting to the cloud no longer hides the local runs.

The constraint this lifts: the studio SPA is a single-origin app that
assumes ONE backend (module-level `/api` base, one WebSocket target, one
`wouter` route tree off `window.location`). Rather than thread a
connection id through every data hook, each pane is an **iframe** — its
own JS realm, so its `/api` base, react-query cache, WS client and router
are naturally independent with almost no change to the data layer.

```
Wails main frame  =  WorkspaceShell (owns ALL native window.go bindings)
  ├─ tab bar / split  (OpenConnection / CloseConnection / GetOpenConnections)
  └─ one <iframe src="/x/<connID>/"> per open connection
        └─ the SAME studio bundle, scoped by an injected
           window.__ITERION_SCOPE__ = "/x/<connID>"
```

Two rules make it work:

1. **A pane never calls `window.go`.** Wails evaluates a binding's result
   callback into the MAIN frame, so an IPC call from an iframe would hang.
   `isDesktop()` therefore returns `false` inside a scoped pane; the pane
   behaves like browser-mode against its demuxed `/x/<id>/api`, and reaches
   everything it needs over HTTP through the demux proxy.

2. **The asset proxy demultiplexes `/x/<connID>/*`** ([asset_proxy.go](../cmd/iterion-desktop/asset_proxy.go)
   `serveScoped`) against a registry of open connections
   ([connections.go](../cmd/iterion-desktop/connections.go)):
   - `/x/<id>/api/…`     → the connection's backend (loopback daemon, or
     cloud with an injected `Bearer` + refresh-on-401), path rewritten to
     `/api/…`.
   - `/x/<id>/_ws/info`  → `{ ws_base, needs_ticket }` so the pane can dial
     its WebSocket **directly** at the backend (WS can't cross the Wails
     asset origin, which 501s upgrades).
   - `/x/<id>/_ws/ticket`→ a single-use cloud WS ticket (empty for local).
   - `/x/<id>/…` (else)  → the SPA `index.html` with the scope injected.
   Shared `/assets/*` are referenced at the root and served once, unscoped.

The registry (`App.conns`) holds one `activeConn` per open connection —
local daemons are shared and kept alive across close (in-flight runs
survive); each cloud connection carries its own token jar + background
refresh loop. The legacy single-connection fields (`serverURL`,
`cloudJar`) still drive the unscoped `/api/*` path for plain browser mode.

Client-side: [lib/scope.ts](../studio/src/lib/scope.ts) reads the injected
scope; `apiBase()` prefixes it onto `/api` (one choke point in
[api/client.ts](../studio/src/api/client.ts) `apiRequest` also scopes any
literal `/api/…` path); `resolveScopedWsUrl()` resolves the WS base +
ticket over the `_ws/*` endpoints; and [main.tsx](../studio/src/main.tsx)
branches on `isWailsHosted() && !isScopedPane()` (synchronous, stable at
first paint) to render the [WorkspaceShell](../studio/src/workspace/WorkspaceShell.tsx)
in the main frame vs. the normal `App` (under a `wouter` base of the
scope) in a pane.

Auth in a cloud pane is desktop-owned: the demux strips the request Cookie
and injects a Bearer refreshed off the OS-held jar, so a pane never runs its
own `/auth/refresh` (it can't — no cookie); a 401 that reaches the pane means
the desktop's refresh already failed, and recovery is driven from the shell
(`cloud:auth-expired` → re-login → reload). The Browser-Live (CDP) pane
resolves its WS through the same `_ws/*` path as every other pane, so it
works for a local pane; a cloud CDP pane additionally needs the CDP endpoint
to accept the `?ticket=` WS credential (follow-up).

## Key decisions

### Why a real `127.0.0.1:<random>` server PLUS Wails AssetServer reverse-proxy

The naive design — load the SPA directly on `http://127.0.0.1:<port>/` —
fails because Wails injects `/wails/runtime.js` and `/wails/ipc.js` ONLY
into HTML responses served by its AssetServer. A page on a different
origin gets no runtime, so `window.go.main.App.*` is `undefined`,
`isDesktop()` returns `false`, and every desktop view silently un-mounts
into "browser mode". (See [ADR-002](adr/002-desktop-assetserver-proxy.md)
for the full investigation that surfaced this in iteration 10.)

The current design solves this by hosting the studio SPA via the Wails
AssetServer's `Handler` (`cmd/iterion-desktop/asset_proxy.go`), so the
WebView's main origin stays on the AssetServer URL for the lifetime of
the app. The handler serves every non-`/api` SPA/static request from the
GUI binary's own `pkg/server.StaticFS` embed; Wails injects the runtime
into the handler's HTML response; bindings remain reachable; and only
`/api/*` HTTP requests are reverse-proxied to the selected loopback
server.

WebSocket upgrades (`/api/ws`, `/api/ws/runs/*`) **cannot** flow through
the AssetServer handler — Wails' AssetServer rejects WS with `501` by
design — so the SPA dials them directly at
`ws://127.0.0.1:<port>/api/ws...`. In current local desktop builds
`GetSessionToken` returns an empty string for SPA compatibility, so the
WS dialer omits any token query parameter. This is the only surface where
the SPA touches the loopback origin directly; normal page/static traffic
is served by the AssetServer handler from the GUI embed, and REST traffic
stays on the AssetServer origin while being proxied to `/api/*`. We keep
a real loopback server (rather than running everything inside the
AssetServer) because:

- `pkg/server` stays 1:1 between CLI (`iterion studio`) and desktop;
- WebSockets keep working without any AssetServer shim;
- the selected server binds to loopback only and runs through the same
  local-editor `DisableAuth=true` path as `iterion studio`, while Origin
  checks still protect browser-initiated writes and WS upgrades.

### Local desktop auth model

By default the desktop host uses per-project headless daemons. On startup
and project switch it finds or launches `iterion-desktop --server-only
--project=<dir>` and points the AssetServer handler's `/api/*` proxy and
direct WS URL at that daemon. Multiple project daemons may coexist; when
the GUI switches projects it attaches to or spawns the new project's
daemon and leaves the previous daemon alive so background runs can
continue.

The in-process `ServerHost` path in `cmd/iterion-desktop/server_host.go`
is now an escape hatch: set `ITERION_DESKTOP_ATTACH_DAEMON=0` (also
`false`, `no`, or `off`) to opt out, or rely on it as the fallback when
daemon attach/spawn fails. That path starts `cli.RunStudio` on
`127.0.0.1` with `Port=-1` and `NoBrowser=true`.

Both the default headless daemon and the in-process fallback use
`cli.RunStudio`, which constructs `pkg/server.Config` with
`DisableAuth=true`. Local desktop mode therefore does not mint a
per-launch session token and does not install cookie/query-token
middleware. The AssetServer handler proxies only `/api/*` HTTP requests to
the loopback server, forces Host to the loopback target, and rewrites
`Origin` to that loopback origin; it does not attach or strip desktop
session cookies. SPA/static requests never reach the loopback server in
current builds; they are served from the GUI embed.

`GetSessionToken` remains exposed on the Wails binding because the SPA's
desktop WS helper calls it together with `GetServerURL`. Today it returns
`""`, and the SPA skips adding a token parameter when the value is empty.
If a future hosted-auth desktop flow wires a real token into this binding,
that token handling should be documented with the auth implementation that
consumes it.

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
| `cmd/iterion-desktop/server_host.go` | In-process `cli.RunStudio` fallback when daemon attach is disabled or fails |
| `cmd/iterion-desktop/config.go` | User config (no build tag — testable) |
| `cmd/iterion-desktop/keychain.go` | go-keyring wrapper |
| `cmd/iterion-desktop/external_cli.go` | claude/codex/git detection |
| `cmd/iterion-desktop/path_fix_darwin.go` | macOS PATH fix |
| `cmd/iterion-desktop/path_fix_other.go` | No-op on non-darwin |
| `cmd/iterion-desktop/updater*.go` | Ed25519-signed auto-update |
| `cmd/iterion-desktop/asset_proxy.go` | Wails AssetServer handler: GUI-embedded SPA/static plus `/api/*` reverse-proxy to the selected loopback server (see ADR-002) |
| `studio/src/lib/desktopBridge.ts` | Typed `window.go.main.App.*` wrappers |
| `studio/src/hooks/useDesktop.ts` | React hook for desktop state |
| `studio/src/views/Welcome/` | First-run wizard |
| `studio/src/views/SettingsDialog/` | Local API keys, backends, projects, storage, appearance, updates, and about |
| `studio/src/views/account/` | Cloud account settings, API keys, OAuth connections, and tokens |
| `studio/src/views/ProjectSwitcher/` | Cmd+P modal |

## Backwards compatibility

The `iterion` CLI is unchanged. `iterion studio` still listens on 4891
with the same local `DisableAuth=true` behaviour — the CLI flag flows are untouched. Two binaries
ship from the same module: `iterion` (Cobra CLI) and `iterion-desktop`
(Wails app). Both share `pkg/cli`, `pkg/server`, the SPA, and the run
store on disk.
