// Per-credential usage — super-admin, cross-tenant. Mirrors
// pkg/server/cred_usage_routes.go handleAdminCredentialUsage (#641): what a
// single credential cost across every tenant it served — the platform view no
// team page can show. Registered only when the usage counter is wired, so a
// call on a server without it 404s → FeatureUnavailableError.
//
// The response separates metered_usd (a real invoice) from estimated_usd (what
// a subscription's calls WOULD have cost); the two are never summed — the
// server states this in the payload's `nature`, and callers must keep them
// apart. Shapes come from the generated OpenAPI types (schema.ts).

import { FeatureUnavailableError, guard404, request } from "./client";
import type { components } from "./schema";

export { FeatureUnavailableError };

export type CredentialUsageListView = components["schemas"]["credentialUsageListView"];
export type CredentialUsageView = components["schemas"]["credentialUsageView"];

// The server answers ONE question per call; fingerprint and repo are mutually
// exclusive (it 400s if both are given), and an absent tier defaults to the
// platform tier. month is YYYY-MM (defaults to the current month).
export interface AdminCredentialUsageQuery {
  tier?: "team" | "org" | "pool" | "platform";
  fingerprint?: string;
  repo?: string;
  month?: string; // YYYY-MM
}

export function getAdminCredentialUsage(
  q: AdminCredentialUsageQuery = {},
): Promise<CredentialUsageListView> {
  const sp = new URLSearchParams();
  // fingerprint and repo are exclusive server-side; forward whatever the
  // caller set and let the API reject an illegal pair with its typed error.
  if (q.fingerprint) sp.set("fingerprint", q.fingerprint);
  if (q.repo) sp.set("repo", q.repo);
  // tier only bites when neither fingerprint nor repo is set, but the server
  // ignores it in those cases, so forwarding it unconditionally is harmless
  // and keeps the query honest to what the caller asked.
  if (q.tier) sp.set("tier", q.tier);
  if (q.month) sp.set("month", q.month);
  const s = sp.toString();
  return guard404("admin-credentials-usage", () =>
    request<CredentialUsageListView>(`/admin/credentials/usage${s ? `?${s}` : ""}`),
  );
}
