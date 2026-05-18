/**
 * Shared WebSocket URL resolver. Used by both the run-console WS hook
 * (useRunWebSocket) and the file-watcher / project-switch WS client
 * (api/ws), each of which previously had a near-identical copy.
 *
 * Desktop note: in Wails the SPA loads on the AssetServer origin
 * (wails:// or http://wails.localhost) but AssetServer rejects WS
 * upgrades. We dial the embedded HTTP server directly via
 * getDesktopWsBase, which carries a session token (the only auth
 * channel that survives the cross-origin boundary). In browser / CLI
 * mode we share an origin with the API so a relative ws:// URL works.
 */

import { getDesktopWsBase, isDesktop, isWailsHosted } from "./desktopBridge";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

export interface BuildWsUrlDeps {
  /** API base prefix, e.g. "/api" or "http://localhost:4891/api". */
  baseUrl: string;
  /** Returns true when running inside the Wails desktop wrapper. */
  isDesktop: () => boolean;
  /** Returns true when the SPA is hosted by Wails AssetServer (wails:// origin). */
  isWailsHosted: () => boolean;
  /** Resolve the absolute desktop WS URL for an absolute path including the API prefix. */
  getDesktopWsBase: (absolutePath: string) => Promise<string | null>;
  /** window.location.protocol — "http:" or "https:". */
  locationProtocol: string;
  /** window.location.host — e.g. "localhost:4891". */
  locationHost: string;
}

/**
 * DI-friendly URL builder. `suffix` is the WS path AFTER the API base
 * (e.g. "/ws", "/ws/runs/abc"). Throws "desktop bindings not ready"
 * when isWailsHosted() is true but getDesktopWsBase() returned null —
 * callers are expected to schedule a reconnect on this transient.
 */
export async function buildWsUrlWith(
  deps: BuildWsUrlDeps,
  suffix: string,
): Promise<string> {
  if (deps.isDesktop()) {
    const desktopUrl = await deps.getDesktopWsBase(`${deps.baseUrl}${suffix}`);
    if (desktopUrl) return desktopUrl;
  }
  if (deps.isWailsHosted()) {
    throw new Error("desktop bindings not ready");
  }
  if (deps.baseUrl.startsWith("http")) {
    return deps.baseUrl.replace(/^http/, "ws") + suffix;
  }
  const proto = deps.locationProtocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${deps.locationHost}${deps.baseUrl}${suffix}`;
}

/** Production wrapper using the module-level defaults. */
export async function buildWsUrl(suffix: string): Promise<string> {
  return buildWsUrlWith(
    {
      baseUrl: BASE_URL,
      isDesktop,
      isWailsHosted,
      getDesktopWsBase,
      locationProtocol: window.location.protocol,
      locationHost: window.location.host,
    },
    suffix,
  );
}
