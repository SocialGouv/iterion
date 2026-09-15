import { afterEach, describe, expect, it, vi } from "vitest";
import {
  completeOAuthAuthorize, deleteOAuth, refreshOAuth, renameOAuth,
  startOAuthAuthorize, uploadOAuthCredentials,
} from "./byok";

vi.mock("@/lib/scope", () => ({ apiBase: () => "/api" }));
afterEach(() => vi.unstubAllGlobals());

describe("OAuth fallback entry requests", () => {
  it("addresses the selected rank for every mutation and preserves labels", async () => {
    const request = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", request);
    const scope = { teamId: "team/one" };
    await startOAuthAuthorize("claude_code", scope, 2);
    await completeOAuthAuthorize("claude_code", { code: "code#state" }, scope, " Work & tests ", 2);
    await uploadOAuthCredentials("claude_code", "{}", scope, " Work & tests ", 2);
    await renameOAuth("claude_code", "Renamed", scope, 2);
    await refreshOAuth("claude_code", scope, 2);
    await deleteOAuth("claude_code", scope, 2);
    expect(request).toHaveBeenCalledTimes(6);
    const calls = request.mock.calls as unknown as [string, RequestInit][];
    expect(calls.map(([, init]) => init.method)).toEqual(["POST", "POST", "POST", "PATCH", "POST", "DELETE"]);
    for (const [path] of calls) {
      const url = new URL(path, "https://test.invalid");
      expect(url.pathname).toContain("/api/teams/team%2Fone/oauth/claude_code");
      expect(url.searchParams.get("rank")).toBe("2");
    }
    for (const index of [1, 2]) {
      const call = calls[index];
      if (!call) throw new Error("missing connect request");
      expect(new URL(call[0], "https://test.invalid").searchParams.get("account_label")).toBe("Work & tests");
    }
  });

  it("keeps legacy callers on the primary entry", async () => {
    const request = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", request);
    await refreshOAuth("claude_code");
    expect(request).toHaveBeenCalledWith("/api/me/oauth/claude_code/refresh", expect.objectContaining({ method: "POST" }));
  });
});
