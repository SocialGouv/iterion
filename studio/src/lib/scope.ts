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

/**
 * STUDIO_BASE is the path prefix every studio route sits under, so that "/"
 * can serve the product home instead. Its Go twin is deeplink.StudioBase
 * (pkg/deeplink), which is what server-built links use; the two are held
 * together by TestStudioBaseMatchesTheStudioConstant.
 *
 * Routes are NOT written with this prefix: wouter's <Router base> applies it,
 * so `navigate("/runs")` and `<Route path="/runs">` stay as they are. Code
 * that reads window.location directly bypasses that and must use studioBase().
 */
export const STUDIO_BASE = "/studio";

// scopePrefix returns "/x/<connID>" for a workspace pane, or "" otherwise.
export function scopePrefix(): string {
  const s = (globalThis as { __ITERION_SCOPE__?: unknown }).__ITERION_SCOPE__;
  return typeof s === "string" && s.startsWith("/x/") ? s : "";
}

// isScopedPane reports whether this document is a workspace pane iframe.
export function isScopedPane(): boolean {
  return scopePrefix() !== "";
}

/**
 * studioBase is the absolute path the studio is mounted at in THIS document:
 * "/studio" in a browser, "/x/<id>/studio" in a workspace pane. It is what
 * wouter's <Router base> is given, and what any code building an absolute URL
 * from window.location must prepend.
 */
export function studioBase(): string {
  return scopePrefix() + STUDIO_BASE;
}

/**
 * rootRoute builds a wouter target for a path that lives OUTSIDE the studio
 * base but inside this document's scope — /login, /invitations/accept, a
 * /config share link.
 *
 * The leading "~" tells wouter to ignore whatever router base it is called
 * from; the scope prefix is then re-applied by hand, because "~" escapes ALL
 * of it and a workspace pane's /x/<id> is not part of the studio base — a bare
 * "~/login" would send a pane to the desktop shell's own origin root.
 */
export function rootRoute(path: string): string {
  return "~" + scopePrefix() + path;
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
