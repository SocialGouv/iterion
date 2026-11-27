// Personal Access Token REST client. Mirrors pkg/server/pat_routes.go.
// URL space is /api/me/tokens — deliberately distinct from /api/me/api-keys
// (BYOK LLM provider keys).

import { ApiError, FeatureUnavailableError, guard404 } from "./client";
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

// The server lists the live and disabled tokens; includeRevoked asks for the
// revoked and expired ones too, which never authenticate again.
export async function listMyTokens(opts?: { includeRevoked?: boolean }): Promise<PersonalAccessToken[]> {
  return guard404("pats", async () => {
    const r = await apiGet(
      "/api/me/tokens",
      opts?.includeRevoked ? { query: { include_revoked: true } } : undefined,
    );
    return r.tokens ?? [];
  });
}

// What a token can do now, revocation first: a revoked token is dead
// whatever else its row says. `now` is injectable for tests.
export type TokenStatus = "active" | "disabled" | "expired" | "revoked";

export function tokenStatus(t: PersonalAccessToken, now: Date = new Date()): TokenStatus {
  if (t.revoked_at) return "revoked";
  if (t.expires_at && new Date(t.expires_at).getTime() < now.getTime()) return "expired";
  if (t.disabled_at) return "disabled";
  return "active";
}

export function createMyToken(input: CreatePATInput): Promise<CreatePATResponse> {
  // Body type-checked against the spec's createPATReq.
  return guard404("pats", () => apiPost("/api/me/tokens", { body: input }));
}

// What a revoke ended: the pending invitations the token had emailed
// (ADR-174), which live no longer than it, and why the sweep could not take
// every one out when it could not.
export type TokenRevocation = components["schemas"]["patRevokedView"];

export async function revokeMyToken(tokenID: string): Promise<TokenRevocation> {
  // A 204 is a revoke that ended nothing: apiRequest answers it undefined.
  const r = (await guard404("pats", () =>
    apiDelete("/api/me/tokens/{token_id}", { params: { token_id: tokenID } }),
  )) as TokenRevocation | undefined;
  return sweepOf(r);
}

// sweepOf reads what a revoke or a purge ended, a 204 (undefined) being a
// sweep that ended nothing.
function sweepOf(r: TokenRevocation | undefined): TokenRevocation {
  return {
    invitations_revoked: r?.invitations_revoked ?? [],
    invitations_revoked_elsewhere: r?.invitations_revoked_elsewhere,
    invitations_error: r?.invitations_error,
  };
}

// What a regenerate answers: the replacement and its plaintext (shown ONCE),
// the token it replaced, what revoking that one ended, and revoke_error when
// the replaced token could not be revoked — it then still authenticates.
export type TokenRegeneration = components["schemas"]["patRegeneratedView"];

export function regenerateMyToken(tokenID: string): Promise<TokenRegeneration> {
  return guard404("pats", () =>
    apiPost("/api/me/tokens/{token_id}/regenerate", { params: { token_id: tokenID } }),
  );
}

// Suspend a token: it authenticates nothing and the invitations it emailed
// are refused until it is enabled again — they are not ended.
export function disableMyToken(tokenID: string): Promise<PersonalAccessToken> {
  return guard404("pats", () =>
    apiPost("/api/me/tokens/{token_id}/disable", { params: { token_id: tokenID } }),
  );
}

export function enableMyToken(tokenID: string): Promise<PersonalAccessToken> {
  return guard404("pats", () =>
    apiPost("/api/me/tokens/{token_id}/enable", { params: { token_id: tokenID } }),
  );
}

// Remove a REVOKED token's row (its audit lines stay). The server finishes
// the revoke's invitation sweep first and answers what that ended. A sweep it
// could not finish keeps the row and answers 409 pat_sweep_unfinished, its
// body naming what it did end: that reads as a sweep with invitations_error
// set (the row is still listed, and deleting again finishes it).
export async function purgeMyToken(tokenID: string): Promise<TokenRevocation> {
  try {
    const r = (await guard404("pats", () =>
      apiPost("/api/me/tokens/{token_id}/purge", { params: { token_id: tokenID } }),
    )) as TokenRevocation | undefined;
    return sweepOf(r);
  } catch (e) {
    if (e instanceof ApiError && e.errorCode === "pat_sweep_unfinished" && e.body) {
      const swept = sweepOf(e.body as unknown as TokenRevocation);
      return { ...swept, invitations_error: swept.invitations_error ?? e.detail };
    }
    throw e;
  }
}
