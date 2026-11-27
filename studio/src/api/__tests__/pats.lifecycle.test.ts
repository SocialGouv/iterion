// The lifecycle helpers of the PAT client: the list asks for the dead tokens
// only on request, a purge's 204 reads as a sweep that ended nothing, and a
// token's status puts revocation first.
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/api/typed", async () => {
  const actual = await vi.importActual<typeof import("@/api/typed")>("@/api/typed");
  return { ...actual, apiGet: vi.fn(), apiPost: vi.fn() };
});

import { apiGet, apiPost } from "@/api/typed";
import { ApiError } from "@/api/client";
import { listMyTokens, purgeMyToken, tokenStatus, type PersonalAccessToken } from "@/api/pats";

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
  vi.mocked(apiPost).mockReset();
});

const base: PersonalAccessToken = {
  id: "p1",
  name: "ci",
  token_last4: "abcd",
  created_at: "2026-11-01T00:00:00Z",
  user_id: "u",
};

describe("listMyTokens", () => {
  it("asks for the revoked tokens only when told to", async () => {
    vi.mocked(apiGet).mockResolvedValue({ tokens: [] } as never);
    await listMyTokens();
    expect(vi.mocked(apiGet)).toHaveBeenLastCalledWith("/api/me/tokens", undefined);
    await listMyTokens({ includeRevoked: true });
    expect(vi.mocked(apiGet)).toHaveBeenLastCalledWith("/api/me/tokens", {
      query: { include_revoked: true },
    });
  });
});

describe("purgeMyToken", () => {
  it("reads a 204 as a purge that ended nothing", async () => {
    vi.mocked(apiPost).mockResolvedValueOnce(undefined as never);
    await expect(purgeMyToken("p1")).resolves.toEqual({
      invitations_revoked: [],
      invitations_revoked_elsewhere: undefined,
      invitations_error: undefined,
    });
    expect(vi.mocked(apiPost)).toHaveBeenCalledWith("/api/me/tokens/{token_id}/purge", {
      params: { token_id: "p1" },
    });
  });
});

describe("purgeMyToken when the sweep could not finish", () => {
  it("names what the sweep did end and the reason, rather than throwing", async () => {
    const ended = [{ id: "i1", email: "lucas@example.org", team_id: "ta", role: "member" }];
    vi.mocked(apiPost).mockRejectedValueOnce(
      new ApiError(409, "API error 409: token p1 is kept", "pat_sweep_unfinished", "token p1 is kept", {
        error: "token p1 is kept",
        error_code: "pat_sweep_unfinished",
        invitations_revoked: ended,
        invitations_error: "revoking invitation i2 failed",
      }),
    );
    await expect(purgeMyToken("p1")).resolves.toEqual({
      invitations_revoked: ended,
      invitations_revoked_elsewhere: undefined,
      invitations_error: "revoking invitation i2 failed",
    });
  });

  it("rethrows any other refusal", async () => {
    vi.mocked(apiPost).mockRejectedValueOnce(
      new ApiError(409, "API error 409: not revoked", "pat_not_revoked", "not revoked", { error_code: "pat_not_revoked" }),
    );
    await expect(purgeMyToken("p1")).rejects.toThrow("not revoked");
  });
});

describe("tokenStatus", () => {
  const now = new Date("2026-11-10T00:00:00Z");
  it.each([
    ["active", base],
    ["disabled", { ...base, disabled_at: "2026-11-02T00:00:00Z" }],
    ["expired", { ...base, expires_at: "2026-11-05T00:00:00Z" }],
    ["expired", { ...base, disabled_at: "2026-11-02T00:00:00Z", expires_at: "2026-11-05T00:00:00Z" }],
    ["revoked", { ...base, disabled_at: "2026-11-02T00:00:00Z", revoked_at: "2026-11-03T00:00:00Z" }],
    ["active", { ...base, expires_at: "2026-12-01T00:00:00Z" }],
  ] as const)("reads %s", (want, token) => {
    expect(tokenStatus(token, now)).toBe(want);
  });
});
