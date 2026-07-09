/**
 * scope.ts — desktop workspace "pane scope".
 *
 * In the desktop workspace each connection (local project or cloud) is shown
 * in its own pane: an <iframe src="/x/<connID>/"> loading this same studio
 * bundle. The demux asset proxy injects `window.__ITERION_SCOPE__ = "/x/<id>"`
 * into that scoped index.html, so a pane knows which backend it belongs to and
 * prefixes every /api call + resolves its WebSocket base accordingly.
 *
 * A pane MUST NOT use the Wails IPC (window.go): a call made from an iframe
 * has its result callback evaluated into the MAIN frame, so the pane's promise
 * would hang. Everything a pane needs (API, WS base, WS ticket) is therefore
 * reached over HTTP through the demux proxy — never through window.go. The
 * native bindings stay owned by the workspace shell (the main frame).
 *
 * Non-scoped contexts (browser mode, or the desktop workspace shell itself)
 * have no __ITERION_SCOPE__, so scopePrefix() is "" and every helper degrades
 * to the historical single-origin behaviour.
 */

// scopePrefix returns "/x/<connID>" for a workspace pane, or "" otherwise.
export function scopePrefix(): string {
  const s = (globalThis as { __ITERION_SCOPE__?: unknown }).__ITERION_SCOPE__;
  return typeof s === "string" && s.startsWith("/x/") ? s : "";
}

// isScopedPane reports whether this document is a workspace pane iframe.
export function isScopedPane(): boolean {
  return scopePrefix() !== "";
}

// apiBase returns the /api prefix for this context: "/x/<id>/api" in a pane,
// "/api" (or the VITE_API_URL override) otherwise. Captured once at module
// load — the scope is fixed for the life of the realm.
export function apiBase(): string {
  const configured = import.meta.env.VITE_API_URL ?? "/api";
  return scopePrefix() + configured;
}

interface ScopedWsInfo {
  ws_base: string;
  needs_ticket: boolean;
}

let cachedWsInfo: ScopedWsInfo | null = null;

async function fetchWsInfo(): Promise<ScopedWsInfo> {
  if (cachedWsInfo) return cachedWsInfo;
  const res = await fetch(`${scopePrefix()}/_ws/info`, { credentials: "include" });
  if (!res.ok) throw new Error(`_ws/info ${res.status}`);
  cachedWsInfo = (await res.json()) as ScopedWsInfo;
  return cachedWsInfo;
}

async function mintScopedTicket(): Promise<string> {
  const res = await fetch(`${scopePrefix()}/_ws/ticket`, {
    method: "POST",
    credentials: "include",
  });
  if (!res.ok) return "";
  const j = (await res.json()) as { ticket?: string };
  return j.ticket ?? "";
}

/**
 * resolveScopedWsUrl turns a scoped WS path (e.g. "/x/<id>/api/ws/runs/abc")
 * into an absolute ws://|wss:// URL dialable directly at the pane's backend —
 * WebSocket upgrades can't traverse the Wails asset origin, so the pane learns
 * the real backend ws base from /x/<id>/_ws/info and (for cloud) mints a
 * single-use ticket from /x/<id>/_ws/ticket per dial. Both are same-origin
 * HTTP through the demux proxy, so no window.go is involved.
 */
export async function resolveScopedWsUrl(fullScopedPath: string): Promise<string> {
  const prefix = scopePrefix();
  const backendPath = fullScopedPath.startsWith(prefix)
    ? fullScopedPath.slice(prefix.length) // "/x/<id>/api/ws/…" → "/api/ws/…"
    : fullScopedPath;
  const info = await fetchWsInfo();
  const u = new URL(info.ws_base + backendPath);
  if (info.needs_ticket) {
    const ticket = await mintScopedTicket();
    if (ticket) u.searchParams.set("ticket", ticket);
  }
  return u.toString();
}
