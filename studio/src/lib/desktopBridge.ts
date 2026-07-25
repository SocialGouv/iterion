// desktopBridge.ts — typed wrappers for the Wails Go bindings exposed by
// cmd/iterion-desktop/bindings.go. When the SPA runs in plain browser
// mode (e.g. served by `iterion studio`), `isDesktop()` returns false and
// every wrapper rejects with a stable error message so the UI can render
// "Desktop only" hints without crashing.
//
// The shape of `window.go.main.App.*` mirrors the Go method names exactly.

import { isScopedPane } from "./scope";

export interface AppInfo {
  version: string;
  commit: string;
  os: string;
  arch: string;
  license: string;
  homepage: string;
  issue_tracker: string;
  documentation: string;
}

export interface Project {
  id: string;
  name: string;
  // kind discriminates the connection: "local" (a directory served by the
  // embedded server / per-project daemon) or "cloud" (a remote iterion
  // instance reached over the authenticating loopback proxy). Empty/absent
  // is treated as "local" for backwards compatibility with v1 configs.
  kind?: "local" | "cloud";
  dir: string;
  store_dir?: string;
  last_opened: string; // ISO timestamp
  color?: string;
  // Cloud-connection fields (kind === "cloud" only).
  cloud_url?: string;
  cloud_email?: string;
}

// isCloudConnection reports whether a Project entry is a remote cloud
// connection (mirrors Go's Project.IsCloud).
export function isCloudConnection(p: Project): boolean {
  return p.kind === "cloud";
}

export interface SecretStatus {
  key: string;
  stored: boolean;
  shadowed: boolean;
}

export interface CLIStatus {
  name: string;
  found: boolean;
  path?: string;
  version?: string;
  install_url: string;
}

export interface Release {
  version: string;
  url: string;
  size: number;
  sha256: string;
  ed25519: string;
  release_notes_url: string;
  released_at: string;
}

// CloudUserSummary is the identity the desktop returns after a cloud login —
// enough to name the connection; the full org tree comes from /api/auth/me
// through the proxy.
export interface CloudUserSummary {
  id: string;
  email: string;
  name?: string;
  is_super_admin: boolean;
  active_org_id?: string;
  active_team_id?: string;
}

export interface CloudProvider {
  name: string;
  display: string;
}

export interface CloudProviders {
  signup_mode: string;
  providers: CloudProvider[];
}

// Internal: shape of the window.go object Wails injects.
interface WailsBindings {
  GetServerURL: () => Promise<string>;
  GetSessionToken: () => Promise<string>;
  GetWsTicket: () => Promise<string>;
  GetDaemonURLForStore: (storePath: string) => Promise<string>;
  SaveTextFile: (suggestedFilename: string, content: string) => Promise<string>;
  SaveBinaryFile: (suggestedFilename: string, base64Data: string) => Promise<string>;
  GetAppInfo: () => Promise<AppInfo>;
  Quit: () => Promise<void>;
  SetWindowTitle: (title: string) => Promise<void>;
  OpenExternal: (url: string) => Promise<void>;
  RevealInFinder: (path: string) => Promise<void>;
  ListProjects: () => Promise<Project[]>;
  ListConnections: () => Promise<Project[]>;
  GetCurrentProject: () => Promise<Project | null>;
  AddProject: (dir: string) => Promise<Project>;
  AddProjectSilently: (dir: string) => Promise<Project>;
  RemoveProject: (id: string) => Promise<void>;
  RemoveConnection: (id: string) => Promise<void>;
  SwitchProject: (id: string) => Promise<void>;
  // Workspace (multi-connection): open/close a connection as a live pane and
  // list the currently-open ones. OpenConnection activates the backend (spawn
  // local daemon / hydrate cloud jar) without changing any "current" pointer.
  OpenConnection: (id: string) => Promise<Project>;
  CloseConnection: (id: string) => Promise<void>;
  GetOpenConnections: () => Promise<string[]>;
  PickProjectDirectory: () => Promise<string>;
  // Cloud connections (password + SSO auth).
  ConnectCloud: (cloudURL: string, email: string, password: string) => Promise<Project>;
  ConnectCloudSSO: (cloudURL: string, provider: string) => Promise<Project>;
  LoginCloud: (connID: string, email: string, password: string) => Promise<CloudUserSummary>;
  LogoutCloud: (connID: string) => Promise<void>;
  ListCloudProviders: (cloudURL: string, email: string) => Promise<CloudProviders>;
  GetKnownSecretKeys: () => Promise<string[]>;
  GetSecretStatuses: () => Promise<SecretStatus[]>;
  SetSecret: (key: string, value: string) => Promise<void>;
  DeleteSecret: (key: string) => Promise<void>;
  DetectExternalCLIs: (force: boolean) => Promise<CLIStatus[]>;
  IsFirstRunPending: () => Promise<boolean>;
  MarkFirstRunDone: () => Promise<void>;
  CheckForUpdate: () => Promise<Release | null>;
  DownloadAndApplyUpdate: () => Promise<void>;
}

declare global {
  interface Window {
    // Wails injects window.go.main.App at runtime in desktop mode only.
    go?: { main?: { App?: WailsBindings } };
    // Wails runtime helpers (events, etc).
    runtime?: {
      EventsOn: (event: string, cb: (data: unknown) => void) => () => void;
      EventsOff: (event: string) => void;
      EventsEmit: (event: string, ...args: unknown[]) => void;
    };
  }
}

export function isDesktop(): boolean {
  // A workspace pane (iframe at /x/<connID>/) must NOT use the Wails IPC:
  // Wails evaluates a binding's result callback into the MAIN frame, so a call
  // from the iframe would hang. Wails may still inject window.go into the
  // iframe, so gate explicitly on the scope marker — a scoped pane behaves as
  // browser-mode against its demuxed /x/<id>/api, with WS resolved over HTTP.
  if (isScopedPane()) return false;
  return (
    typeof window !== "undefined" &&
    !!window.go &&
    !!window.go.main &&
    !!window.go.main.App
  );
}

// isWailsHosted reports whether the SPA is loading from a Wails AssetServer
// origin (wails:// on Mac/Linux, http://wails.localhost on Windows). This is
// orthogonal to isDesktop(): the page can be hosted by Wails even before the
// /wails/ipc.js script has finished injecting `window.go.main.App`. Treat
// this as "we will be in desktop mode soon, don't fall through to a relative
// WS URL that can't possibly resolve".
export function isWailsHosted(): boolean {
  if (typeof window === "undefined") return false;
  const host = window.location.host;
  const proto = window.location.protocol;
  return (
    proto === "wails:" ||
    host === "wails" ||
    host === "wails.localhost"
  );
}

// call invokes the Wails binding identified by `key` with the given args.
// In browser mode it returns a rejected Promise so callers can rely on
// the wrappers being uniformly async (no synchronous throw).
function call<K extends keyof WailsBindings>(
  key: K,
  ...args: Parameters<WailsBindings[K] extends (...a: infer P) => unknown ? (...a: P) => unknown : never>
): ReturnType<WailsBindings[K] extends (...a: never[]) => infer R ? () => R : never> {
  if (!isDesktop()) {
    return Promise.reject(new Error("Not available in browser mode")) as ReturnType<
      WailsBindings[K] extends (...a: never[]) => infer R ? () => R : never
    >;
  }
  const fn = window.go!.main!.App![key] as (...a: unknown[]) => unknown;
  return fn(...args) as ReturnType<
    WailsBindings[K] extends (...a: never[]) => infer R ? () => R : never
  >;
}

// ── Generic API ──────────────────────────────────────────────────────────

export const desktop = {
  isDesktop,

  getServerURL: () => call("GetServerURL"),
  getSessionToken: () => call("GetSessionToken"),
  getWsTicket: () => call("GetWsTicket"),
  // getDaemonURLForStore resolves the daemon URL serving the given iterion
  // store path. Used by RunsPanel's "in other locations" section to deep-link
  // cross-daemon runs without 404ing. Returns "" when no live daemon is found
  // and no fallback is available — the SPA reads "" as "stay on current
  // daemon".
  getDaemonURLForStore: (storePath: string) =>
    call("GetDaemonURLForStore", storePath),
  saveTextFile: (suggestedFilename: string, content: string) =>
    call("SaveTextFile", suggestedFilename, content),
  saveBinaryFile: (suggestedFilename: string, base64Data: string) =>
    call("SaveBinaryFile", suggestedFilename, base64Data),
  getAppInfo: () => call("GetAppInfo"),
  quit: () => call("Quit"),
  setWindowTitle: (title: string) => call("SetWindowTitle", title),
  openExternal: (url: string) => call("OpenExternal", url),
  revealInFinder: (path: string) => call("RevealInFinder", path),

  // Projects / connections (the unified MRU list holds both local projects
  // and remote cloud connections; listConnections is the semantic alias).
  listProjects: () => call("ListProjects"),
  listConnections: () => call("ListConnections"),
  getCurrentProject: () => call("GetCurrentProject"),
  addProject: (dir: string) => call("AddProject", dir),
  addProjectSilently: (dir: string) => call("AddProjectSilently", dir),
  removeProject: (id: string) => call("RemoveProject", id),
  removeConnection: (id: string) => call("RemoveConnection", id),
  switchProject: (id: string) => call("SwitchProject", id),
  openConnection: (id: string) => call("OpenConnection", id),
  closeConnection: (id: string) => call("CloseConnection", id),
  getOpenConnections: () => call("GetOpenConnections"),
  pickProjectDirectory: () => call("PickProjectDirectory"),

  // Cloud connections
  connectCloud: (cloudURL: string, email: string, password: string) =>
    call("ConnectCloud", cloudURL, email, password),
  connectCloudSSO: (cloudURL: string, provider: string) =>
    call("ConnectCloudSSO", cloudURL, provider),
  loginCloud: (connID: string, email: string, password: string) =>
    call("LoginCloud", connID, email, password),
  logoutCloud: (connID: string) => call("LogoutCloud", connID),
  listCloudProviders: (cloudURL: string, email: string) =>
    call("ListCloudProviders", cloudURL, email),

  // Secrets
  getKnownSecretKeys: () => call("GetKnownSecretKeys"),
  getSecretStatuses: () => call("GetSecretStatuses"),
  setSecret: (key: string, value: string) => call("SetSecret", key, value),
  deleteSecret: (key: string) => call("DeleteSecret", key),

  // External CLIs
  detectExternalCLIs: (force = false) => call("DetectExternalCLIs", force),

  // First-run
  isFirstRunPending: () => call("IsFirstRunPending"),
  markFirstRunDone: () => call("MarkFirstRunDone"),

  // Updates
  checkForUpdate: () => call("CheckForUpdate"),
  downloadAndApplyUpdate: () => call("DownloadAndApplyUpdate"),
} as const;

// ── WebSocket dialer (desktop) ───────────────────────────────────────────

/**
 * In desktop mode the studio SPA loads from the Wails AssetServer origin
 * (wails:// on Mac/Linux, http://wails.localhost on Windows) so the
 * `window.go.main.App.*` bindings stay reachable. Wails' AssetServer rejects
 * WebSocket upgrades with 501, so the studio's WS clients must dial the
 * embedded HTTP server DIRECTLY at http://127.0.0.1:<port>/api/ws...
 *
 * This helper resolves to that absolute ws:// URL with an auth credential on
 * the query string (the only channel available across this origin boundary —
 * HttpOnly cookies set on the loopback/cloud domain are not sent cross-origin
 * from wails://). For a CLOUD connection it mints a fresh single-use WS ticket
 * per dial (?ticket=), so a long-lived JWT never lands in the URL; it falls
 * back to ?t=<token> when no ticket is available (local mode returns neither).
 * In browser/CLI mode the SPA shares an origin with the API so we hand back a
 * relative URL that the caller's `${proto}//${host}` derivation already handles.
 *
 * The resolved URL is cached per server URL so a project switch
 * (which rebinds the server on a new ephemeral port and triggers
 * WindowReloadApp) naturally invalidates the cache because the page reloads.
 */
let cachedDesktopWsBase: { serverURL: string; wsBase: string } | null = null;

export async function getDesktopWsBase(path: string): Promise<string | null> {
  if (!isDesktop()) {
    console.warn("[desktopBridge] getDesktopWsBase: window.go.main.App not present yet (Wails bindings still loading)");
    return null;
  }
  let serverURL: string;
  let token: string;
  try {
    [serverURL, token] = await Promise.all([
      desktop.getServerURL(),
      desktop.getSessionToken(),
    ]);
  } catch (err) {
    console.warn("[desktopBridge] getDesktopWsBase: binding call rejected", err);
    return null;
  }
  if (!serverURL) {
    console.warn("[desktopBridge] getDesktopWsBase: GetServerURL returned empty (embedded HTTP server not bound — see desktop log for server start failures)");
    return null;
  }
  // Recompute when the server URL changes (project switch rebinds the
  // embedded server on a fresh ephemeral port).
  if (!cachedDesktopWsBase || cachedDesktopWsBase.serverURL !== serverURL) {
    const u = new URL(serverURL);
    u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
    u.pathname = "";
    u.search = "";
    cachedDesktopWsBase = { serverURL, wsBase: u.toString().replace(/\/$/, "") };
  }
  const base = cachedDesktopWsBase.wsBase;
  const u = new URL(base + path);
  // Prefer a single-use WS ticket (cloud) so the access JWT never enters the
  // URL; fall back to ?t=<token> when no ticket is available. Tickets are
  // single-use, so this mints a fresh one per dial. A mint failure degrades to
  // the token rather than breaking the connection.
  let ticket: string;
  try {
    ticket = await desktop.getWsTicket();
  } catch {
    ticket = "";
  }
  if (ticket) {
    u.searchParams.set("ticket", ticket);
  } else if (token) {
    u.searchParams.set("t", token);
  }
  return u.toString();
}

// ── Events ───────────────────────────────────────────────────────────────

/**
 * Subscribe to a Wails event. Returns an unsubscribe function.
 * In browser mode the subscription is a no-op.
 */
export function onDesktopEvent<T = unknown>(
  event: string,
  handler: (data: T) => void,
): () => void {
  if (typeof window === "undefined" || !window.runtime) {
    return () => {};
  }
  return window.runtime.EventsOn(event, (data) => handler(data as T));
}
