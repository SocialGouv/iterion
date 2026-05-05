// desktopBridge.ts — typed wrappers for the Wails Go bindings exposed by
// cmd/iterion-desktop/bindings.go. When the SPA runs in plain browser
// mode (e.g. served by `iterion editor`), `isDesktop()` returns false and
// every wrapper rejects with a stable error message so the UI can render
// "Desktop only" hints without crashing.
//
// The shape of `window.go.main.App.*` mirrors the Go method names exactly.

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
  dir: string;
  store_dir?: string;
  last_opened: string; // ISO timestamp
  color?: string;
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

// Internal: shape of the window.go object Wails injects.
interface WailsBindings {
  GetServerURL: () => Promise<string>;
  GetSessionToken: () => Promise<string>;
  GetAppInfo: () => Promise<AppInfo>;
  Quit: () => Promise<void>;
  OpenExternal: (url: string) => Promise<void>;
  RevealInFinder: (path: string) => Promise<void>;
  ListProjects: () => Promise<Project[]>;
  GetCurrentProject: () => Promise<Project | null>;
  AddProject: (dir: string) => Promise<Project>;
  AddProjectSilently: (dir: string) => Promise<Project>;
  RemoveProject: (id: string) => Promise<void>;
  SwitchProject: (id: string) => Promise<void>;
  PickProjectDirectory: () => Promise<string>;
  ScaffoldProject: (dir: string) => Promise<void>;
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
  return (
    typeof window !== "undefined" &&
    !!window.go &&
    !!window.go.main &&
    !!window.go.main.App
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
  getAppInfo: () => call("GetAppInfo"),
  quit: () => call("Quit"),
  openExternal: (url: string) => call("OpenExternal", url),
  revealInFinder: (path: string) => call("RevealInFinder", path),

  // Projects
  listProjects: () => call("ListProjects"),
  getCurrentProject: () => call("GetCurrentProject"),
  addProject: (dir: string) => call("AddProject", dir),
  addProjectSilently: (dir: string) => call("AddProjectSilently", dir),
  removeProject: (id: string) => call("RemoveProject", id),
  switchProject: (id: string) => call("SwitchProject", id),
  pickProjectDirectory: () => call("PickProjectDirectory"),
  scaffoldProject: (dir: string) => call("ScaffoldProject", dir),

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
 * In desktop mode the editor SPA loads from the Wails AssetServer origin
 * (wails:// on Mac/Linux, http://wails.localhost on Windows) so the
 * `window.go.main.App.*` bindings stay reachable. Wails' AssetServer rejects
 * WebSocket upgrades with 501, so the editor's WS clients must dial the
 * embedded HTTP server DIRECTLY at http://127.0.0.1:<port>/api/ws...
 *
 * This helper resolves to that absolute ws:// URL with the session token
 * on the query string (the only auth channel available across this origin
 * boundary — HttpOnly cookies set on the loopback domain are not sent
 * cross-origin from wails://). In browser/CLI mode the SPA shares an origin
 * with the API so we hand back a relative URL that the caller's
 * `${proto}//${host}` derivation already handles.
 *
 * The resolved URL is cached per server URL so a project switch
 * (which rebinds the server on a new ephemeral port and triggers
 * WindowReloadApp) naturally invalidates the cache because the page reloads.
 */
let cachedDesktopWsBase: { serverURL: string; wsBase: string } | null = null;

export async function getDesktopWsBase(path: string): Promise<string | null> {
  if (!isDesktop()) return null;
  let serverURL: string;
  let token: string;
  try {
    [serverURL, token] = await Promise.all([
      desktop.getServerURL(),
      desktop.getSessionToken(),
    ]);
  } catch {
    return null;
  }
  if (!serverURL) return null;
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
  if (token) u.searchParams.set("t", token);
  return u.toString();
}

// resetDesktopWsCache is exposed for tests + future "explicit reload" paths.
// Project-switch reload already invalidates the in-memory cache via the
// page reload, but tests that swap the bindings stub between runs need a
// way to drop it manually.
export function resetDesktopWsCache(): void {
  cachedDesktopWsBase = null;
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
