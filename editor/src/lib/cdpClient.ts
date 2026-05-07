// cdpClient.ts — minimal Chrome DevTools Protocol client over the
// editor's `/api/runs/:id/browser/cdp` WebSocket proxy. CDP is a
// JSON-RPC 2.0 dialect: messages have an `id`, `method`, `params`
// shape on the way out and `id` + `result`|`error` on the way back.
// Notifications (no `id`) carry domain events (Page.screencastFrame,
// Network.responseReceived, …).
//
// Wire format on our proxy is BinaryMessage frames containing UTF-8
// JSON — the proxy is byte-pumping a `--remote-debugging-pipe`
// upstream that already speaks JSON. We don't try to be smart about
// chunking: Chromium emits one whole JSON object per pipe write, so
// one BinaryMessage = one CDP message.

export type CDPMessage = {
  id?: number;
  method?: string;
  params?: Record<string, unknown>;
  result?: Record<string, unknown>;
  error?: { code: number; message: string };
  sessionId?: string;
};

type Pending = {
  resolve: (result: Record<string, unknown>) => void;
  reject: (err: Error) => void;
};

type Listener = (params: Record<string, unknown>) => void;

export interface CDPClientOptions {
  // Run id used to build the WS URL. Required.
  runId: string;
  // Browser session id (registered server-side by the runtime when
  // Chromium starts). Required.
  sessionId: string;
  // Override the WS base. Defaults to current page origin so the
  // SPA's existing reverse proxy / Wails AssetServer routing kicks
  // in. Wails desktop sets this to the explicit `http://127.0.0.1:<port>`
  // because iframes inside Wails AssetServer can't open WS via
  // relative paths (the AssetServer rejects WS upgrades).
  serverBase?: string;
  // Optional session token query (?t=<token>) for the WS handshake.
  // Local mode leaves it empty; cloud + desktop pass the cookie /
  // bindings.GetSessionToken() value here.
  sessionToken?: string;
}

export class CDPClient {
  private ws: WebSocket | null = null;
  private nextId = 1;
  private pending = new Map<number, Pending>();
  private listeners = new Map<string, Set<Listener>>();
  private connected = false;
  private closed = false;
  private connectPromise: Promise<void> | null = null;

  constructor(private opts: CDPClientOptions) {}

  // connect resolves once the WS handshake completes. Idempotent —
  // calling twice yields the same promise.
  connect(): Promise<void> {
    if (this.connectPromise) return this.connectPromise;
    this.connectPromise = new Promise<void>((resolve, reject) => {
      const url = this.buildURL();
      const ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      this.ws = ws;
      ws.onopen = () => {
        this.connected = true;
        resolve();
      };
      ws.onerror = () => {
        if (!this.connected) reject(new Error("cdp: ws connect failed"));
      };
      ws.onclose = () => {
        this.closed = true;
        this.connected = false;
        // Reject all in-flight requests so callers don't hang.
        for (const p of this.pending.values()) {
          p.reject(new Error("cdp: ws closed"));
        }
        this.pending.clear();
      };
      ws.onmessage = (evt) => this.handleMessage(evt.data);
    });
    return this.connectPromise;
  }

  // send issues a CDP method call and resolves with the `result`
  // map (or rejects on protocol-level error). Bytes are sent as
  // utf-8 JSON over a binary frame to mirror the upstream pipe.
  async send(
    method: string,
    params: Record<string, unknown> = {},
  ): Promise<Record<string, unknown>> {
    if (this.closed) throw new Error("cdp: client closed");
    await this.connect();
    if (!this.ws) throw new Error("cdp: ws not initialised");
    const id = this.nextId++;
    const msg = JSON.stringify({ id, method, params });
    return new Promise<Record<string, unknown>>((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      try {
        this.ws!.send(new TextEncoder().encode(msg));
      } catch (err) {
        this.pending.delete(id);
        reject(err instanceof Error ? err : new Error(String(err)));
      }
    });
  }

  // on subscribes to a CDP notification (e.g. "Page.screencastFrame").
  // Returns an unsubscribe function.
  on(method: string, cb: Listener): () => void {
    let set = this.listeners.get(method);
    if (!set) {
      set = new Set<Listener>();
      this.listeners.set(method, set);
    }
    set.add(cb);
    return () => {
      set!.delete(cb);
    };
  }

  // close shuts the WS and rejects any in-flight calls.
  close(): void {
    this.closed = true;
    if (this.ws && this.ws.readyState <= WebSocket.OPEN) {
      this.ws.close(1000, "client closed");
    }
  }

  private buildURL(): string {
    const baseRaw = this.opts.serverBase ?? window.location.origin;
    // Replace http(s) with ws(s).
    const base = baseRaw.replace(/^http/, "ws");
    const path = `/api/runs/${encodeURIComponent(this.opts.runId)}/browser/cdp`;
    const qs = new URLSearchParams({ session: this.opts.sessionId });
    if (this.opts.sessionToken) qs.set("t", this.opts.sessionToken);
    return `${base}${path}?${qs.toString()}`;
  }

  private handleMessage(data: ArrayBuffer | Blob | string): void {
    const decode = (b: ArrayBuffer | string): string =>
      typeof b === "string" ? b : new TextDecoder().decode(b);
    const handle = (text: string) => {
      let msg: CDPMessage;
      try {
        msg = JSON.parse(text) as CDPMessage;
      } catch {
        return;
      }
      if (typeof msg.id === "number") {
        const pending = this.pending.get(msg.id);
        if (!pending) return;
        this.pending.delete(msg.id);
        if (msg.error) {
          pending.reject(new Error(`cdp ${msg.error.code}: ${msg.error.message}`));
        } else {
          pending.resolve(msg.result ?? {});
        }
        return;
      }
      if (msg.method) {
        const set = this.listeners.get(msg.method);
        if (set) {
          for (const cb of set) cb(msg.params ?? {});
        }
      }
    };
    if (data instanceof ArrayBuffer) {
      handle(decode(data));
    } else if (data instanceof Blob) {
      data.arrayBuffer().then((b) => handle(decode(b)));
    } else {
      handle(decode(data));
    }
  }
}
