// revokeMyToken answers what a revoke ended. A 204 — the common answer, a
// revoke that ended nothing — reaches it as undefined from the transport and
// must read as an empty list, never as a missing one.
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/api/typed", async () => {
  const actual = await vi.importActual<typeof import("@/api/typed")>("@/api/typed");
  return { ...actual, apiDelete: vi.fn() };
});

import { apiDelete } from "@/api/typed";
import { revokeMyToken } from "@/api/pats";

beforeEach(() => {
  vi.mocked(apiDelete).mockReset();
});

describe("revokeMyToken", () => {
  it("reads a 204 as a revoke that ended nothing", async () => {
    vi.mocked(apiDelete).mockResolvedValueOnce(undefined as never);
    await expect(revokeMyToken("p1")).resolves.toEqual({ invitations_revoked: [], invitations_error: undefined });
    expect(vi.mocked(apiDelete)).toHaveBeenCalledWith("/api/me/tokens/{token_id}", { params: { token_id: "p1" } });
  });

  it("passes the ended invitations and the sweep's error through", async () => {
    const ended = [{ id: "i1", email: "lucas@example.org", team_id: "ta", role: "admin" }];
    vi.mocked(apiDelete).mockResolvedValueOnce({ invitations_revoked: ended, invitations_error: "listing failed" } as never);
    await expect(revokeMyToken("p1")).resolves.toEqual({ invitations_revoked: ended, invitations_error: "listing failed" });
  });
});
