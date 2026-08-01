// The model registry: every model iterion knows about, crossed with the
// credentials THIS host holds, what each model can do, and what it costs.
//
// Mirrors pkg/modelcatalog (served at GET /api/models). It is what turns the
// launch form's free-text model field into an actual picker: without it the
// only hints were the detected providers' suggested models, which said nothing
// about capabilities, price, or whether a model was reachable at all.

import { request } from "./client";

export interface ModelEntry {
  // Canonical "provider/model-id" — exactly what a `model:` field or a
  // model_overrides entry takes.
  spec: string;
  // The spec's provider prefix (the API dialect), NOT necessarily the vendor
  // holding the credential.
  provider: string;
  model: string;
  // The detect provider whose credential unlocks this spec. Differs from
  // `provider` for façade endpoints — GLM speaks the Anthropic API but bills
  // to z.ai.
  credential_provider: string;
  // Where the capability values came from.
  source: "aggregator" | "curated" | string;

  context_window: number;
  reasoning: boolean;
  // The hard capability gate: a model without tool-calling cannot drive board
  // tools, skills or run introspection, so an agent node on it is broken
  // rather than merely degraded.
  tool_call: boolean;
  temperature: boolean;
  // Whether `reasoning_effort: ultracode` holds here — it is reliable only on
  // claude-opus-4-8 and silently degrades to plain xhigh elsewhere (C089).
  ultracode_capable: boolean;

  // Per-million-token USD rates a run would actually be charged at. Read them
  // only when price_known is true: a zero means "no published price", never
  // "free".
  input_cost_per_m?: number;
  output_cost_per_m?: number;
  price_known: boolean;

  usable: boolean;
  unusable_reason?: string;
  // Backends that can drive this spec right now, in host preference order.
  backends?: string[] | null;
  // The credential that would be used, by NAME (e.g. "ANTHROPIC_API_KEY").
  credential_source?: string;
  recommended?: boolean;
}

export interface ModelCatalog {
  models: ModelEntry[];
  recommended_spec?: string;
  resolved_default_backend?: string;
  backends?:
    | {
        name: string;
        available: boolean;
        auth: string;
        sources?: string[] | null;
        hints?: string[] | null;
      }[]
    | null;
  refreshed?: boolean;
  refresh_error?: string;
}

export interface FetchModelsOptions {
  // Extra specs to resolve IN ADDITION to the curated set — typically the DSL
  // defaults of the bot's own nodes, which may sit outside it. They never
  // narrow the result: a picker restricted to the models already in use is the
  // one list from which no new choice can be made.
  extraSpecs?: string[];
  // Re-probe host credentials AND re-fetch the model-spec aggregator.
  refresh?: boolean;
  signal?: AbortSignal;
}

export async function fetchModels(
  opts: FetchModelsOptions = {},
): Promise<ModelCatalog> {
  const params = new URLSearchParams();
  for (const spec of opts.extraSpecs ?? []) {
    const s = spec.trim();
    // ${VAR} literals are DSL placeholders, not model ids — the server would
    // 400 on the ones without a "/" and resolve nonsense for the rest.
    if (s && !s.includes("${")) params.append("spec", s);
  }
  if (opts.refresh) params.set("refresh", "1");
  const qs = params.toString();
  // Route through the shared `request` wrapper for the same silent
  // 401 → refresh → replay handling every other /api/* call gets.
  return request<ModelCatalog>(`/models${qs ? `?${qs}` : ""}`, {
    method: "GET",
    signal: opts.signal,
    cache: opts.refresh ? "no-store" : "default",
    headers: opts.refresh ? { "Cache-Control": "no-cache" } : undefined,
  });
}

// formatContextWindow renders a token count compactly (1M, 200K, 4096) and "—"
// when unknown. Mirrors the CLI's column so both surfaces read alike.
export function formatContextWindow(n: number): string {
  if (!n || n <= 0) return "—";
  if (n % 1_000_000 === 0) return `${n / 1_000_000}M`;
  if (n % 1_000 === 0) return `${n / 1_000}K`;
  return String(n);
}

// formatModelPrice renders the per-million-token rates. An unpriced model
// shows "—": a zero rate means no source published one, never that it is free.
export function formatModelPrice(m: ModelEntry): string {
  if (!m.price_known) return "—";
  const n = (v: number | undefined) => {
    const x = v ?? 0;
    // Sub-dollar rates need decimals; whole-dollar ones read better without.
    return x < 1 ? x.toFixed(2).replace(/0+$/, "").replace(/\.$/, "") : String(x);
  };
  return `$${n(m.input_cost_per_m)} / $${n(m.output_cost_per_m)} per Mtok`;
}

// backendForModel names the backend that can actually DRIVE a spec, or "" when
// the caller should leave the node's own `backend:` alone.
//
// Choosing a model is not a free choice of one field: a node pinned to
// `backend: "claude_code"` cannot run an OpenAI spec, and re-targeting only
// the model leaves a run that dies at its first node. The registry already
// says which backends reach a spec on this host, so a surface that offers a
// model must send the backend with it.
//
// `preferred` (typically the host's resolved default) wins when it is one of
// them, so the answer stays the least surprising of the valid ones.
export function backendForModel(
  m: ModelEntry | undefined,
  preferred?: string,
): string {
  const list = (m?.backends ?? []).filter(Boolean);
  if (list.length === 0) return "";
  if (preferred && list.includes(preferred)) return preferred;
  return list[0] ?? "";
}

// modelCapabilityWarning names the reason a model is a bad fit for an agent
// node, or null when there is none. Ordered by severity: unreachable and
// tool-less are hard breakages, ultracode is a silent downgrade.
//
// `wantsUltracode` should be true when the node (or session) runs at
// reasoning_effort: ultracode.
export function modelCapabilityWarning(
  m: ModelEntry | undefined,
  opts: { wantsUltracode?: boolean } = {},
): { level: "blocking" | "warning"; message: string } | null {
  if (!m) return null;
  if (!m.usable) {
    return {
      level: "blocking",
      message:
        m.unusable_reason ||
        "no credential on this host can reach this model — the run will fail at the first node",
    };
  }
  if (!m.tool_call) {
    return {
      level: "blocking",
      message:
        "this model cannot call tools — the agent loses the board, skills and run introspection",
    };
  }
  if (opts.wantsUltracode && !m.ultracode_capable) {
    return {
      level: "warning",
      message:
        "ultracode holds only on claude-opus-4-8; here it degrades to plain xhigh",
    };
  }
  return null;
}
