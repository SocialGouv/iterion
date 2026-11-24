// Inbound-webhook REST client. Mirrors pkg/server/webhooks_routes.go.
//
// Token plaintext is shown ONCE at create or rotate — the server only
// persists hash + last4 + fingerprint. The UI must surface the plaintext
// in a token-once panel and then drop it from memory.

import { FeatureUnavailableError, guard404, request } from "./client";

// Re-export so views that already import {FeatureUnavailableError}
// alongside webhook helpers (WebhooksTab, …) keep working without
// touching their import lists.
export { FeatureUnavailableError };

// Every provider is accepted by the backend — see pkg/server/webhooks_routes.go's
// supportedProviders. "connector" is a connector package's ingress (ADR-168):
// created against a connection, delivered to /api/webhooks/c/{id}.
export type WebhookProvider = "gitlab" | "github" | "forgejo" | "generic" | "connector";

export interface WebhookRate {
  rate: number;
  burst: number;
}

export interface WebhookConfig {
  id: string;
  tenant_id: string;
  name: string;
  provider: WebhookProvider;
  enabled: boolean;
  token_last4: string;
  // The connector ingress binding (ADR-168 §3/§6), serialized on every
  // row: which connection receives, its connector and installation, the
  // pinned contract and where the secret comes from.
  connector_connection_id?: string;
  connector?: string;
  installation?: string;
  contract_digest?: string;
  secret_origin?: string;
  fingerprint?: string;
  bot_ids: string[];
  wildcard_bots?: boolean;
  default_bot_id?: string;
  project_allowlist?: string[];
  event_allowlist?: string[];
  author_allowlist?: string[];
  forge_base_url?: string;
  rate_limit: WebhookRate;
  monthly_call_limit?: number;
  launch_vars?: Record<string, string>;
  key_overrides?: Record<string, string>;
  created_by: string;
  created_at: string;
  updated_at: string;
  last_used_at?: string;
  rotated_at?: string;
}

export interface WebhookWithToken {
  config: WebhookConfig;
  token: string;
  // On a create, the api keys / stored secrets its pins name that the server
  // could not re-check once the row was stored; the webhook exists and the
  // token has no second copy, so the answer stays a 201 and this receipt
  // asks the operator to re-read those records and clear a pin naming a
  // deleted one. Absent when every pin was vouched for.
  pins_unreconciled?: string[];
}

export interface WebhookDelivery {
  id: string;
  tenant_id: string;
  webhook_id: string;
  provider: WebhookProvider;
  idempotency_key: string;
  event_kind?: string;
  event_action?: string;
  project_path?: string;
  subject_id?: string;
  subject_sha?: string;
  payload_hash?: string;
  status: string;
  bot_id?: string;
  run_id?: string;
  error?: string;
  source_ip?: string;
  received_at: string;
  launched_at?: string;
  // The outcome of the run this delivery launched (#1501) — what the run
  // DID, as opposed to `status`/`error`, which are the delivery's own
  // verdict. Stamped by the server's run-outcome spine; absent until the run
  // reaches a terminal status. `run_id` above is the tuple's id half.
  last_run_status?: string;
  last_run_error?: string;
  last_run_error_code?: string;
  last_run_at?: string;
}

export interface CreateWebhookInput {
  name: string;
  provider: WebhookProvider;
  bot_ids?: string[];
  wildcard_bots?: boolean;
  default_bot_id?: string;
  project_allowlist?: string[];
  event_allowlist?: string[];
  author_allowlist?: string[];
  forge_base_url?: string;
  rate_limit?: WebhookRate;
  monthly_call_limit?: number;
  launch_vars?: Record<string, string>;
  key_overrides?: Record<string, string>;
  enabled?: boolean;
  // The connector ingress binding (ADR-168 §3/§6): set, the inbound
  // handler is the connector lane and the secret is minted (or the
  // vendor's, when the package declares it).
  connector_connection_id?: string;
  installation?: string;
  vendor_secret?: string;
  contract_digest?: string;
}

export interface UpdateWebhookInput {
  name?: string;
  enabled?: boolean;
  default_bot_id?: string;
  bot_ids?: string[];
  wildcard_bots?: boolean;
  project_allowlist?: string[];
  event_allowlist?: string[];
  author_allowlist?: string[];
  forge_base_url?: string;
  rate_limit?: WebhookRate;
  monthly_call_limit?: number;
  launch_vars?: Record<string, string>;
  key_overrides?: Record<string, string>;
  // The connector ingress binding (ADR-168 §3/§6): set, the inbound
  // handler is the connector lane and the secret is minted (or the
  // vendor's, when the package declares it).
  connector_connection_id?: string;
  installation?: string;
  vendor_secret?: string;
  contract_digest?: string;
}

export async function listWebhooks(teamID: string): Promise<WebhookConfig[]> {
  return guard404("webhooks", async () => {
    const r = await request<{ webhooks: WebhookConfig[] }>(
      `/teams/${encodeURIComponent(teamID)}/webhooks`,
    );
    return r.webhooks ?? [];
  });
}

export async function createWebhook(
  teamID: string,
  input: CreateWebhookInput,
): Promise<WebhookWithToken> {
  return guard404("webhooks", () =>
    request<WebhookWithToken>(`/teams/${encodeURIComponent(teamID)}/webhooks`, {
      method: "POST",
      body: JSON.stringify(input),
    }),
  );
}

export async function updateWebhook(
  teamID: string,
  webhookID: string,
  input: UpdateWebhookInput,
): Promise<WebhookConfig> {
  return guard404("webhooks", () =>
    request<WebhookConfig>(
      `/teams/${encodeURIComponent(teamID)}/webhooks/${encodeURIComponent(webhookID)}`,
      { method: "PATCH", body: JSON.stringify(input) },
    ),
  );
}

export async function deleteWebhook(teamID: string, webhookID: string): Promise<void> {
  await guard404("webhooks", () =>
    request<void>(
      `/teams/${encodeURIComponent(teamID)}/webhooks/${encodeURIComponent(webhookID)}`,
      { method: "DELETE" },
    ),
  );
}

export async function rotateWebhook(
  teamID: string,
  webhookID: string,
): Promise<WebhookWithToken> {
  return guard404("webhooks", () =>
    request<WebhookWithToken>(
      `/teams/${encodeURIComponent(teamID)}/webhooks/${encodeURIComponent(webhookID)}/rotate`,
      { method: "POST" },
    ),
  );
}

export async function listWebhookDeliveries(
  teamID: string,
  webhookID: string,
): Promise<WebhookDelivery[]> {
  return guard404("webhooks", async () => {
    const r = await request<{ deliveries: WebhookDelivery[] }>(
      `/teams/${encodeURIComponent(teamID)}/webhooks/${encodeURIComponent(webhookID)}/deliveries`,
    );
    return r.deliveries ?? [];
  });
}

// ---- pure helpers (covered by vitest) ----

/**
 * inboundWebhookURL builds the absolute URL an external forge POSTs to.
 * Origin defaults to window.location.origin; tests pass an explicit
 * origin so they don't depend on jsdom.
 *
 * Backend route: /api/webhooks/{provider}/{webhook_id}, and
 * /api/webhooks/c/{webhook_id} for a connector ingress.
 */
export function inboundWebhookURL(
  provider: WebhookProvider,
  webhookID: string,
  origin?: string,
): string {
  const o =
    origin ??
    (typeof window !== "undefined" ? window.location.origin : "https://iterion.local");
  const segment = provider === "connector" ? "c" : encodeURIComponent(provider);
  return `${o.replace(/\/$/, "")}/api/webhooks/${segment}/${encodeURIComponent(webhookID)}`;
}

// providerSetupSnippet returns a copy-pastable instruction block per
// provider so the operator can wire the forge without leaving the UI.
export function providerSetupSnippet(
  provider: WebhookProvider,
  url: string,
  token: string,
): { title: string; steps: string[]; example?: string } {
  switch (provider) {
    case "gitlab":
      return {
        title: "Set up in GitLab",
        steps: [
          'In the project: Settings → Webhooks.',
          `Paste the URL above as "URL".`,
          `Paste the token below as "Secret token".`,
          'Tick "Merge request events" and "Comments".',
          'Save webhook.',
        ],
        example: `# inbound URL\n${url}\n# secret token (paste in GitLab "Secret token")\n${token}`,
      };
    case "github":
      return {
        title: "Set up in GitHub",
        steps: [
          'In the repo: Settings → Webhooks → Add webhook.',
          `Paste the URL above as "Payload URL".`,
          'Content type: application/json.',
          `Paste the token below as "Secret".`,
          'Choose "Let me select individual events" → tick "Pull requests".',
          'Add webhook.',
        ],
        example: `# Payload URL\n${url}\n# Secret\n${token}`,
      };
    case "forgejo":
      return {
        title: "Set up in Forgejo / Gitea",
        steps: [
          'In the repo: Settings → Webhooks → Add Webhook.',
          `Paste the URL above as "Target URL".`,
          'Content Type: application/json.',
          `Paste the token below as "Secret".`,
          'Trigger On: choose "Custom Events" → tick Pull Requests / Issues comments.',
          'Add Webhook.',
        ],
        example: `# Target URL\n${url}\n# Secret\n${token}`,
      };
    case "generic":
      return {
        title: "Trigger with a curl",
        steps: [
          "Send a JSON POST carrying the X-Iterion-Webhook-Token header.",
          "Any payload shape is accepted; iterion exposes its fields as launch vars.",
        ],
        example: `curl -X POST '${url}' \\\n  -H 'Content-Type: application/json' \\\n  -H 'X-Iterion-Webhook-Token: ${token}' \\\n  -d '{"hello":"world"}'`,
      };
    case "connector":
      // A connector ingress verifies as its package declares: the secret is
      // either minted here (shown once) or the vendor's own, pasted at creation.
      return {
        title: "Set up in the vendor",
        steps: [
          "Paste the URL above in the vendor's webhook settings — the connector's setup guide says where.",
          token
            ? "Paste the secret below where the vendor asks for its signing secret."
            : "The vendor's own signing secret, pasted when the ingress was created, verifies its deliveries.",
        ],
        example: token ? `# URL\n${url}\n# Secret\n${token}` : `# URL\n${url}`,
      };
  }
}
