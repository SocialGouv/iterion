// Plugins API client — the plugin registry (embedded builtins +
// ~/.iterion/plugins) surfaced for the studio Plugins view. Mirrors
// pkg/plugin.View and the GET/POST /api/v1/plugins endpoints.

import { apiBase } from "@/lib/scope";

const BASE = apiBase().replace(/\/$/, "");

// PluginConfigField mirrors pkg/plugin.ConfigField — one declared, user-settable
// setting (like a Firefox add-on preference).
export interface PluginConfigField {
  key: string;
  label?: string;
  type?: "string" | "bool" | "int" | "float" | "enum" | "secret";
  description?: string;
  default?: string;
  options?: string[];
  required?: boolean;
}

export interface PluginView {
  name: string;
  version?: string;
  description?: string;
  author?: string;
  enabled: boolean;
  builtin: boolean;
  kinds: string[];
  // Config UI: the declared fields, the current non-secret values, and which
  // secret fields currently have a value (their value is never sent down).
  config_schema?: PluginConfigField[];
  config_values?: Record<string, string>;
  config_secret_set?: string[];
}

// RewriterInfo mirrors pkg/plugin.RewriterInfo — one command-output compressor
// contributed by the plugin.
export interface RewriterInfo {
  id: string;
  sandbox_mount?: string;
  timeout_ms?: number;
  argv?: string[];
}

// MCPServerInfo mirrors pkg/plugin.MCPServerInfo — a contributed MCP server
// and how it is reached (command+args for stdio, url for http/sse).
export interface MCPServerInfo {
  name: string;
  transport: string;
  command?: string;
  args?: string[];
  url?: string;
}

// HookInfo mirrors pkg/plugin.HookInfo — the raw shell commands the plugin
// fires on one claude_code hook event, shown verbatim so the operator can vet
// them before enabling.
export interface HookInfo {
  event: string;
  commands?: string[];
}

// LifecycleInfo mirrors pkg/plugin.LifecycleInfo — the manifest's index /
// refresh command strings.
export interface LifecycleInfo {
  index?: string;
  refresh?: string;
}

// PluginDetail mirrors pkg/plugin.Detail — the full projection behind the
// plugin detail view (GET /api/v1/plugins/{name}).
export interface PluginDetail {
  view: PluginView;
  readme?: string;
  auto_index: boolean;
  rewriters?: RewriterInfo[];
  mcp_servers?: MCPServerInfo[];
  skills?: string[];
  commands?: string[];
  agents?: string[];
  hooks?: HookInfo[];
  lifecycle?: LifecycleInfo;
  dir?: string;
}

// PluginLifecycleResult is the response of POST .../lifecycle/{phase}. ok:false
// means the command ran and failed — output/error still carry what happened.
export interface PluginLifecycleResult {
  name: string;
  phase: string;
  ok: boolean;
  output: string;
  truncated?: boolean;
  error?: string;
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

// getPluginDetail fetches the full detail projection for one plugin.
export async function getPluginDetail(name: string): Promise<PluginDetail> {
  return send(`/v1/plugins/${encodeURIComponent(name)}`);
}

// runPluginLifecycle runs a plugin's index/refresh manifest command in the
// server's workspace. Super-admin only server-side; rejected in cloud mode.
export async function runPluginLifecycle(
  name: string,
  phase: "index" | "refresh",
): Promise<PluginLifecycleResult> {
  return send(`/v1/plugins/${encodeURIComponent(name)}/lifecycle/${phase}`, {
    method: "POST",
  });
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

// setPluginConfig persists a plugin's config values. Super-admin only. A secret
// field left blank keeps its prior value server-side. Returns the refreshed view.
export async function setPluginConfig(
  name: string,
  values: Record<string, string>,
): Promise<PluginView> {
  return send(`/v1/plugins/${encodeURIComponent(name)}/config`, {
    method: "PUT",
    body: JSON.stringify({ values }),
  });
}
