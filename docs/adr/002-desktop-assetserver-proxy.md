# ADR-002: Desktop editor SPA hosted via Wails AssetServer reverse-proxy

- **Status**: Accepted
- **Date**: 2026-05-05
- **Authors**: feature-dev
- **Scope**: `cmd/iterion-desktop/`, `pkg/server/`, `editor/`

## Context

The desktop binary (`iterion-desktop`) is a thin native shell around the
existing CLI editor server (`pkg/server`). Its value proposition lives in
the **Wails IPC bindings** (`window.go.main.App.*`): project switcher,
secrets/keychain, CLI detection, auto-update — none of which the editor
SPA can do on its own.

The original design (Phase 1 of the desktop plan) loaded a tiny embedded
"bootstrap stub" in the Wails AssetServer, called `GetServerURL` /
`GetSessionToken` on `window.go.main.App`, and **navigated the WebView**
to `http://127.0.0.1:<port>/?t=<token>` so the editor SPA loaded from the
embedded HTTP server.

That design has a fatal architectural flaw discovered in iteration 10 of
the desktop feature review:

1. Wails injects `/wails/runtime.js` and `/wails/ipc.js` — the scripts that
   define `window.go`, `window.runtime`, `window.WailsInvoke` — **only into
   HTML responses served by the AssetServer itself**
   (`vendor/.../assetserver.go:200-220`).
2. Custom URL schemes (`wails://`) and the `wails.localhost` host are the
   only origins the AssetServer intercepts; everything else, including
   `http://127.0.0.1:<random>`, is passed through to the WebView's default
   network stack (`vendor/.../windows/frontend.go:652-658`).
3. `BindingsAllowedOrigins` was unset, so the message dispatcher
   (`vendor/.../originvalidator/originValidator.go:46-58`) would have
   blocked any binding call from the localhost origin even if the runtime
   had somehow loaded.
4. Concretely: after the bootstrap-stub redirected the window to
   `http://127.0.0.1:<port>/`, `window.go` was `undefined`, `isDesktop()`
   returned `false`, and the SPA quietly fell back to browser mode — every
   desktop-only view (`<Welcome>`, `<Settings>`, `<ProjectSwitcher>`,
   `<MissingCLIBanner>`) was un-mounted, every native-menu emit silently
   discarded, every `desktop.*` call rejected with "Not available in
   browser mode".

Tests missed this because `editor/src/__tests__/desktopBridge.test.ts`
stubs `window.go` directly, and `bindings_project_test.go` uses a fake
`recordingServer` plus a stubbed `windowReloader`. Neither exercises the
real cross-origin runtime injection that Wails actually performs at
runtime.

## Decision

The Wails AssetServer hosts the editor SPA via a reverse-proxy
`http.Handler` (`cmd/iterion-desktop/asset_proxy.go`). The WebView's main
origin stays on the AssetServer URL (`wails://wails/` on Mac/Linux,
`http://wails.localhost/` on Windows) for the lifetime of the app, and:

- HTTP traffic (REST API + SPA static assets) is forwarded to the embedded
  `pkg/server` via `httputil.NewSingleHostReverseProxy`. Wails detects the
  `text/html` response on `GET /` and injects `/wails/runtime.js` +
  `/wails/ipc.js`, which is what makes `window.go.main.App.*` reachable to
  the editor SPA — the exact gap that was the production blocker.
- The proxy `Director` rewrites the `Origin` header to the loopback target
  (so `pkg/server`'s `requireSafeOrigin` allowlist passes without having to
  teach it the `wails://` origin scheme) and **always** strips any inbound
  `iterion_session` cookie before re-attaching the canonical token so the
  local server's session middleware authenticates the proxied call. The
  SPA never has to learn or echo the token over the wire.
- WebSocket upgrades (`/api/ws`, `/api/ws/runs/*`) **cannot** flow through
  the proxy: Wails' AssetServer rejects WS upgrades with `501` by design
  (`vendor/.../assetserver.go:110-114`). The editor SPA dials WS endpoints
  directly at `ws://127.0.0.1:<port>/api/ws...` using the absolute URL
  resolved through `getDesktopWsBase()` in
  `editor/src/lib/desktopBridge.ts`, with the session token on the query
  string.
- `pkg/server/session.go` accepts the same `?t=<token>` query string on
  any path (not just bootstrap `GET /`), so cross-origin WS handshakes
  authenticate without the HttpOnly cookie scoped to the loopback domain.
  The non-bootstrap query path **does not** set a cookie — the WS
  handshake is short-lived and shouldn't persist the token in browser
  storage on a non-bootstrap origin.
- `pkg/server/server.go`'s WS-origin allowlist is extended in desktop mode
  (when `cfg.SessionToken != ""`) to include `wails://wails` and
  `http://wails.localhost`, so the upgrader's `CheckOrigin` accepts the
  SPA's true origin. The token gates entry; the origin allowlist is
  defense-in-depth.

The bootstrap-stub directory (`cmd/iterion-desktop/frontend-stub/`) and
the `embed.go` that wired it up are deleted — they're obsolete with the
proxy approach.

## Alternatives considered

### 1. Inject Wails runtime + IPC scripts via the local server

Embed `runtime_prod_desktop.js` and `ipc.js` from `vendor/.../runtime/`
into the desktop binary, serve them at `/wails/runtime.js` +
`/wails/ipc.js` from `pkg/server`, and add `<script>` tags to
`editor/index.html`.

**Rejected**: the runtime needs the `window.wailsbindings` JSON to
populate `window.go.<package>.<class>.<method>`. That JSON is generated by
Wails internals during reflection on the `Bind` list; it's not exposed
through any public API. Without it, `window.go` would be `{}` even with
the runtime loaded. Reverse-engineering the bindings emitter into the
desktop binary would re-implement substantial Wails machinery and have to
track every Wails bump.

### 2. Move all desktop functionality to same-origin `/api/*` endpoints

Refactor every Wails binding (`ListProjects`, `SetSecret`,
`DetectExternalCLIs`, `CheckForUpdate`, …) into HTTP handlers on
`pkg/server`, drop `Bind: []interface{}{app}`, and remove the dependency
on Wails IPC entirely.

**Rejected (for v1)**: large blast radius — every binding, every desktop
test, every frontend caller would change. This is a viable v2 if Wails IPC
ever proves limiting (single-fenced WebView, reflection startup cost), but
the proxy approach lets v1 keep the existing binding surface intact.

### 3. Allow-list the localhost origin via `BindingsAllowedOrigins`, keep
   the redirect

Set `BindingsAllowedOrigins: "http://127.0.0.1:*"` and have the editor's
`index.html` fetch + execute `/wails/runtime.js` + `/wails/ipc.js`
cross-origin from the AssetServer.

**Rejected**: the `wails://` scheme on Mac/Linux is custom and WebKit
disallows cross-scheme `<script>` loads from `http://`. Even on Windows
(`http://wails.localhost`), the runtime.js served by the AssetServer
includes the dynamically-generated `window.wailsbindings = '...'` prefix
with the bindings JSON; embedding the static `vendor/` JS file alone
wouldn't include that prefix, so `window.go` would still be empty.

## Consequences

### Positive

- `window.go.main.App.*` is reachable on the editor SPA in desktop mode.
  Welcome, Settings, ProjectSwitcher, MissingCLIBanner, and the
  native-menu event subscriptions all work as designed.
- The session-cookie flow stays simple for the SPA — the proxy attaches
  the cookie on every forwarded request; the SPA never sees or echoes the
  token.
- `iterion editor` (CLI) is byte-identical to before: `SessionToken` is
  empty, the session middleware is not installed, the WS-origin allowlist
  doesn't get the Wails extensions, the proxy code is desktop-only via
  `//go:build desktop`.
- The reviewer's blocker is resolved without restructuring the binding
  surface or vendoring more Wails internals.

### Negative

- Adds ~150 LOC of proxy code (`asset_proxy.go` + tests).
- The desktop binary now has TWO HTTP servers in the request path: the
  Wails AssetServer (intercepting `wails://` / `wails.localhost`) and the
  embedded `pkg/server`. Latency overhead is one extra in-process hop
  (negligible) but every observability story has to account for both
  layers. We mitigate by keeping the proxy minimal (no caching, no
  body-rewriting, only Director-time header tweaks).
- The session middleware gains a second auth path (?t= query for
  non-bootstrap requests). It's narrowly scoped (the local server still
  rejects without either cookie or matching token) but the surface is
  larger than the cookie-only design. Tests in
  `pkg/server/session_test.go` lock in both paths.
- The WS-origin allowlist grows by two entries when SessionToken is set.
  This is desktop-only and gated by `cfg.SessionToken != ""`, but it does
  introduce two CLI-vs-desktop-divergent strings in `pkg/server`. We mark
  them clearly as "desktop mode" so future edits don't accidentally
  permit `wails://` origins on the CLI server.

### Operational

- `docs/desktop-architecture.md` updated to reflect the proxy + WS
  carve-out.
- `docs/desktop-qa.md` first checkbox now exercises the actual production
  path (bindings reachable on editor SPA), so a regression of the original
  blocker would surface immediately on the next QA pass.
