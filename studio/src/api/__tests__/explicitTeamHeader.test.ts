// @vitest-environment jsdom
// The explicit team header (#2662): a tenant-scoped request carries the
// team THIS TAB published — the server validates it against the session's
// grants — and carries nothing when no team was adopted yet.
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { apiRequest, publishExplicitTeam } from "@/api/client";

let seen: { path: string; team: string }[] = [];

beforeEach(() => {
  seen = [];
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const headers = new Headers(init?.headers);
    seen.push({ path: String(input), team: headers.get("x-iterion-team") ?? "" });
    return new Response(JSON.stringify({ ok: true }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }));
});

afterEach(() => {
  publishExplicitTeam(null);
  vi.unstubAllGlobals();
});

it("a published team travels on tenant-scoped requests", async () => {
  publishExplicitTeam("tb");
  await apiRequest("/api/probe");
  expect(seen[0]?.path).toBe("/api/probe");
  expect(seen[0]?.team).toBe("tb");
});

it("an unadopted tab sends no header and scopes as before", async () => {
  await apiRequest("/api/probe");
  expect(seen[0]?.team).toBe("");
});

it("a caller-supplied header keeps precedence over the published team", async () => {
  publishExplicitTeam("tb");
  await apiRequest("/probe", { headers: { "X-Iterion-Team": "ta" } });
  expect(seen[0]?.team).toBe("ta");
});
