// Plugins API client — the plugin registry (embedded builtins +
// ~/.iterion/plugins) surfaced for the studio Plugins view. Mirrors
// pkg/plugin.View and the GET/POST /api/v1/plugins endpoints.

const BASE = (import.meta.env.VITE_API_URL ?? "/api").replace(/\/$/, "");

export interface PluginView {
  name: string;
  version?: string;
  description?: string;
  author?: string;
  enabled: boolean;
  builtin: boolean;
  kinds: string[];
}

async function send<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    credentials: "include",
    headers: init?.body ? { "Content-Type": "application/json", ...(init?.headers ?? {}) } : init?.headers,
    ...init,
  });
  if (!res.ok) {
    let msg: string | undefined;
    try {
      const body = (await res.json()) as unknown;
      if (body && typeof body === "object") {
        const env = body as { error?: unknown; message?: unknown };
        if (typeof env.error === "string") msg = env.error;
        else if (typeof env.message === "string") msg = env.message;
      }
    } catch {
      // Non-JSON body — fall back to statusText.
    }
    throw new Error(msg || res.statusText || `HTTP ${res.status}`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export async function listPlugins(): Promise<PluginView[]> {
  const res = await send<{ plugins: PluginView[] }>("/v1/plugins");
  return res.plugins ?? [];
}

export async function setPluginEnabled(name: string, enabled: boolean): Promise<void> {
  await send(`/v1/plugins/${encodeURIComponent(name)}/${enabled ? "enable" : "disable"}`, {
    method: "POST",
  });
}

// installPlugin installs a plugin from a git URL or local path. Super-admin
// only on the server (POST /v1/plugins/install, requireSuperAdmin); returns the
// installed plugin's view when the registry could resolve it.
export async function installPlugin(source: string): Promise<{ name: string; plugin?: PluginView }> {
  return send("/v1/plugins/install", {
    method: "POST",
    body: JSON.stringify({ source }),
  });
}

// uninstallPlugin removes an installed (non-builtin) plugin. Super-admin only.
export async function uninstallPlugin(name: string): Promise<void> {
  await send(`/v1/plugins/${encodeURIComponent(name)}`, { method: "DELETE" });
}
