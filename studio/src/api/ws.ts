import { buildWsUrl } from "@/lib/wsUrl";
import type { ServerWsEvent } from "./types";

// File-watcher and project-switch stream — see buildWsUrl for transport
// notes (Wails AssetServer rejects WS upgrades, hence the indirection
// through getDesktopWsBase in desktop mode).
async function deriveWsUrl(): Promise<string> {
  return buildWsUrl("/ws");
}

// The /api/ws channel now carries both file-change events and the
// global `project_switched` broadcast (and any future single-channel
// signals). Subscribers receive the discriminated union; they're
// expected to filter on `event.type`.
type ServerWsHandler = (event: ServerWsEvent) => void;

class FileWatcherClient {
  private ws: WebSocket | null = null;
  private handlers = new Set<ServerWsHandler>();
  private reconnectDelay = 1000;
  private maxDelay = 30000;
  private shouldConnect = false;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private everConnected = false;
  private consecutiveFailures = 0;
  private maxInitialRetries = 3;
  // refCount allows multiple consumers (useFileWatcher, useProjectSwitchListener)
  // to call connect/disconnect independently without trampling each other:
  // the WS dials on the first acquire and tears down only on the last release.
  // Without this, navigating between editor and run views (which mount
  // useFileWatcher conditionally) would race the always-on project_switched
  // listener and silently drop its connection.
  private refCount = 0;

  connect(): void {
    this.refCount++;
    if (this.refCount > 1) return;
    this.shouldConnect = true;
    this.everConnected = false;
    this.consecutiveFailures = 0;
    void this.doConnect();
  }

  disconnect(): void {
    if (this.refCount === 0) return;
    this.refCount--;
    if (this.refCount > 0) return;
    this.shouldConnect = false;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
  }

  subscribe(handler: ServerWsHandler): () => void {
    this.handlers.add(handler);
    return () => this.handlers.delete(handler);
  }

  private async doConnect(): Promise<void> {
    if (!this.shouldConnect) return;
    let url: string;
    try {
      url = await deriveWsUrl();
    } catch {
      // Could not resolve URL (e.g. desktop bindings not ready) — schedule
      // a retry rather than crashing the SPA.
      this.consecutiveFailures++;
      this.scheduleReconnect();
      return;
    }
    if (!this.shouldConnect) return; // disconnect raced the await
    const ws = new WebSocket(url);

    ws.onopen = () => {
      this.everConnected = true;
      this.consecutiveFailures = 0;
      this.reconnectDelay = 1000;
    };

    ws.onmessage = (ev) => {
      try {
        const event = JSON.parse(ev.data) as ServerWsEvent;
        for (const handler of this.handlers) {
          handler(event);
        }
      } catch (err) {
        // Surface protocol drift instead of swallowing — useRunWebSocket
        // does the same and silent JSON.parse failures here previously
        // hid genuine server bugs from devtools.
        // eslint-disable-next-line no-console
        console.warn("[file-watcher ws] dropped message:", err);
      }
    };

    ws.onclose = () => {
      this.ws = null;
      this.consecutiveFailures++;
      this.scheduleReconnect();
    };

    ws.onerror = () => {
      // onclose will fire after this, triggering reconnect logic
      ws.close();
    };

    this.ws = ws;
  }

  private scheduleReconnect(): void {
    if (!this.shouldConnect) return;

    // If we never connected and exhausted initial retries, stop silently.
    // The backend may not support WebSocket (e.g., standalone frontend).
    if (!this.everConnected && this.consecutiveFailures >= this.maxInitialRetries) {
      this.shouldConnect = false;
      return;
    }

    // Defensive: clear any existing armed timer before scheduling a new
    // one. The normal path (onclose → scheduleReconnect) only fires when
    // no timer is armed, but a deriveWsUrl failure inside doConnect can
    // re-enter scheduleReconnect while the previous timer is still in
    // flight, double-scheduling and accumulating backoff.
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.doConnect();
    }, this.reconnectDelay);
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, this.maxDelay);
  }
}

export const fileWatcher = new FileWatcherClient();
