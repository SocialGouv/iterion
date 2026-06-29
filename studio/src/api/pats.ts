// Personal Access Token REST client. Mirrors pkg/server/pat_routes.go.
// URL space is /api/me/tokens — deliberately distinct from /api/me/api-keys
// (BYOK LLM provider keys).

import { FeatureUnavailableError, guard404 } from "./client";
import type { components } from "./schema";
import { apiDelete, apiGet, apiPost } from "./typed";

export { FeatureUnavailableError };

// These ARE the spec's schemas (generated from pkg/pat + the handlers), so the
// responses below need no cast and can't drift from the server.
export type PersonalAccessToken = components["schemas"]["Token"];
export type CreatePATInput = components["schemas"]["createPATReq"];

export interface CreatePATResponse {
  pat: PersonalAccessToken;
  // The plaintext shown ONCE — never re-fetchable.
  token: string;
}

export async function listMyTokens(): Promise<PersonalAccessToken[]> {
  return guard404("pats", async () => {
    const r = await apiGet("/api/me/tokens");
    return r.tokens ?? [];
  });
}

export function createMyToken(input: CreatePATInput): Promise<CreatePATResponse> {
  // Body type-checked against the spec's createPATReq.
  return guard404("pats", () => apiPost("/api/me/tokens", { body: input }));
}

export async function revokeMyToken(tokenID: string): Promise<void> {
  await guard404("pats", () => apiDelete("/api/me/tokens/{token_id}", { params: { token_id: tokenID } }));
}
