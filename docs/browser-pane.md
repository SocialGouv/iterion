# Browser pane

The run console's **Browser tab** renders web content tied to a workflow
run. It has three modes that fall through automatically:

| Mode | When | Source |
|------|------|--------|
| **Live** | A Chromium session is attached to the run | CDP screencast over WebSocket |
| **Time-travel** | The run-console scrubber is parked at a seq | Stored screenshot attachment ≤ that seq |
| **Viewer** | Default | `<iframe>` of a workflow-emitted or user-typed URL |

The tab itself only appears once the run has produced *something*: a
preview URL, a screenshot, a live session, or a manual URL the user
typed. Workflows that never touch the web see no UI change.

## Publishing a URL from a workflow

A tool node prints a single line on stdout:

```
[iterion] preview_url=<url> [kind=<k>] [scope=<s>]
```

Examples:

```sh
echo "[iterion] preview_url=http://localhost:3000 kind=dev-server scope=internal"
echo "[iterion] preview_url=https://my-preview.example.com"
```

- `kind` is a free-form hint: `dev-server`, `deploy`, `artifact-html`.
- `scope=external` (default) loads the URL directly in the iframe and
  relies on the target's framing permissions
  (`X-Frame-Options`, `Content-Security-Policy: frame-ancestors`).
- `scope=internal` routes through `/api/runs/:id/preview`, which strips
  frame-blocking headers and re-frames with a strict CSP sandbox. Only
  use it for URLs the run itself published — in cloud mode the proxy
  refuses RFC1918, link-local, cloud-metadata, and `*.svc.cluster.local`
  addresses to mitigate SSRF.

### Headline result-links (`kind=pr` / `deploy` / `app`)

When `kind` is a **result kind** — `pr`, `deploy`, or `app` — the URL is
additionally surfaced as a prominent, always-visible **result-link** in
the run summary (the run header's result-links bar), like a CI run's
"View deployment" button. The links are deduped by URL and kept in
discovery order, so one run can advertise both a PR and a deploy. They
are event-derived, so a reloaded terminal run rebuilds them from its
event log. A `kind=pr` link is **link-only** (GitHub/GitLab block
iframing, so it never loads in the Browser pane); `deploy`/`app` live
sites keep the Browser pane too. The catalog PR-tail bots
(`docs-refresh`, `feature-dev`, `app-dev`, `branch-improve-loop`,
`whole-improve-loop`) emit `kind=pr` from a deterministic tool node once
`finalize_mr` opens a PR; `app-dev` also emits `kind=deploy` for a
healthy deploy.

## Capturing screenshots

A tool node can also publish a screenshot it took (puppeteer,
wkhtmltoimage, headless chromium, anything that produces a PNG/JPEG):

```
[iterion] preview_screenshot=<absolute-path> [url=<u>] [tool_call_id=<id>]
```

The runtime reads the file from the host filesystem and persists it as
a regular run attachment (the same machinery as user uploads). The
studio surfaces every captured frame in **time-travel mode**: when the
scrubber is parked at seq *N*, the pane shows the most-recent frame
with seq ≤ *N* — useful for retroactively inspecting what the workflow
saw at any point in the run.

## Live mode

Two paths today:

1. **Manual debug attach** (local editor mode only, no Playwright
   required) — click **attach live** in the Browser tab. The studio POSTs
   to `/api/runs/:id/browser/attach`, the local studio server spawns
   Chromium on the host via `--remote-debugging-pipe`, registers a
   session in its in-memory `BrowserRegistry`, and the pane connects via
   the CDP WS proxy. Useful for testing the live UI on a fresh run. The
   cloud `iterion server` path does not currently wire a
   `BrowserRegistry`, so live attach/CDP endpoints are unavailable there.

2. **Auto-attach via Playwright MCP** *(staged for a follow-up PR)* —
   when a workflow declares the Playwright MCP server and runs in a
   sandbox image that ships Chromium (`iterion-sandbox-browser`), the
   runtime will spawn Chromium before the agent starts and inject
   `--cdp-endpoint` into the MCP server args so it shares the same
   browser. The studio's Browser pane flips to live mode automatically.

The C060 IR diagnostic enforces the sandbox/image pairing at compile
time when a sandbox is active — workflows that opt into a sandbox + a
Playwright MCP without the browser image fail validation.

### Wire format

The CDP transport on the wire is:

```mermaid
sequenceDiagram
  participant EDITOR as editor
  participant SRV as iterion server
  participant CHR as chromium<br/>--remote-debugging-pipe

  EDITOR->>SRV: "ws://.../api/runs/{id}/browser/cdp?session=...<br/>BinaryMessage frames"
  SRV-->>EDITOR: "BinaryMessage frames"
  SRV->>CHR: docker exec / host pipe
  CHR-->>SRV: docker exec / host pipe
```

Framing rule: **one WebSocket BinaryMessage = one CDP JSON-RPC
message**. The server re-frames Chromium's null-terminated pipe stream
into discrete WS frames in both directions. The frontend client
(`studio/src/lib/cdpClient.ts`) speaks plain JSON-RPC; it doesn't see
the pipe framing.

### Disabling the pane

The whole feature is gated by a single CLI flag:

```sh
iterion studio --no-browser-pane
```

The flag disables every code path: the iframe proxy, the WS endpoint,
the Chromium runner, and the registry. Useful for emergency lockdown
and for shaving startup latency in environments where the pane is
never used.

## Sandbox images

| Image | Includes Chromium | Use case |
|-------|-------------------|----------|
| `iterion-sandbox-slim` | no | Default, lightweight runs |
| `iterion-sandbox-full` | no | Go/Python/pnpm dev tooling |
| `iterion-sandbox-browser` | **yes** | Workflows that drive a browser via Playwright MCP |

Pin a digest in production. The `:edge` tag tracks main and is
intended for development.

## Cross-surface notes

- **Desktop (Wails)**: the SPA loads on the AssetServer origin; iframes
  inside the SPA cannot open a WS via relative paths because Wails'
  AssetServer rejects WS upgrades. The CDP client honours
  `serverBase + sessionToken` overrides for this case — pass the actual
  loopback base from `GetServerURL` (`http://127.0.0.1:<port>`) and append a
  token only when `GetSessionToken` returns a non-empty value. Current local
  desktop builds return `""` and omit `?t=`, relying on `DisableAuth=true`
  plus loopback/Origin checks.
- **Local web**: SPA in the user's browser, server on localhost. Works
  out of the box; iframe + WS use relative paths.
- **Cloud (k8s)**: the current cloud `iterion server` does not wire a
  `BrowserRegistry` or live CDP attach path, so the Browser pane is
  limited to preview URLs and time-travel screenshots. Cloud live mode
  (for example, launching Chromium in the workflow pod and carrying CDP
  through ingress) is future work, not shipped behavior. The preview
  proxy still enforces strict SSRF rules for any cross-origin URL.
