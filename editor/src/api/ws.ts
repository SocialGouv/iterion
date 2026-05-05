import { getDesktopWsBase, isDesktop } from "@/lib/desktopBridge";
import type { FileEvent } from "./types";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

// deriveWsUrl resolves the absolute WebSocket URL for the file-watcher
// stream. In CLI / browser mode the SPA shares an origin with the API, so a
// relative URL works. In desktop mode the SPA is hosted on Wails'
// AssetServer (wails:// or http://wails.localhost), but Wails rejects WS
// upgrades — so we dial the local server directly with the session token in
// the query (the only auth channel that survives this cross-origin
// boundary; HttpOnly cookies set on the loopback domain are not sent).
async function deriveWsUrl(): Promise<string> {
  if (isDesktop()) {
    const desktopUrl = await getDesktopWsBase("/api/ws");
    if (desktopUrl) return desktopUrl;
  }
  if (BASE_URL.startsWith("http")) {
    return BASE_URL.replace(/^http/, "ws") + "/ws";
  }
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${BASE_URL}/ws`;
}

type FileEventHandler = (event: FileEvent) => void;

class FileWatcherClient {
  private ws: WebSocket | null = null;
  private handlers = new Set<FileEventHandler>();
  private reconnectDelay = 1000;
  private maxDelay = 30000;
  private shouldConnect = false;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private everConnected = false;
  private consecutiveFailures = 0;
  private maxInitialRetries = 3;

  connect(): void {
    this.shouldConnect = true;
    this.everConnected = false;
    this.consecutiveFailures = 0;
    void this.doConnect();
  }

  disconnect(): void {
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

  subscribe(handler: FileEventHandler): () => void {
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
        const event = JSON.parse(ev.data) as FileEvent;
        for (const handler of this.handlers) {
          handler(event);
        }
      } catch {
        // ignore malformed messages
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

    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.doConnect();
    }, this.reconnectDelay);
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, this.maxDelay);
  }
}

export const fileWatcher = new FileWatcherClient();
