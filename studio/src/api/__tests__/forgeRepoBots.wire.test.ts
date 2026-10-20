// PATCH /api/teams/{id}/forge/repo-bots/{integration_id} replaces the
// integration's bot set when `bot_ids` is PRESENT and keeps the stored one
// when it is ABSENT (pkg/server forgeUpdateReq). The body this client builds
// is therefore what decides whether a launch-vars-only write can unbind a bot
// bound since the page loaded — hence a witness at the wire, not at the call.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { updateForgeRepoBots } from "@/api/forgeConnections";

const realFetch = globalThis.fetch;
let calls: { url: string; method?: string; body: Record<string, unknown> }[] = [];

beforeEach(() => {
  calls = [];
  globalThis.fetch = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({
      url: String(url),
      method: init?.method,
      body: JSON.parse(String(init?.body)) as Record<string, unknown>,
    });
    return {
      ok: true,
      status: 200,
      headers: { get: () => null },
      json: async () => ({}),
      text: async () => "{}",
    };
  }) as unknown as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

describe("updateForgeRepoBots — the bot set on the wire", () => {
  it("omits bot_ids entirely for a launch-vars-only write", async () => {
    await updateForgeRepoBots("t1", "i1", undefined, undefined, {
      review_tier: "audit",
      gate_enabled: "false",
    });

    expect(calls[0]?.url).toBe("/api/teams/t1/forge/repo-bots/i1");
    expect(calls[0]?.method).toBe("PATCH");
    // The KEY must be absent: a null or an empty list is a 400, and a present
    // list takes the server's replace path.
    expect(Object.keys(calls[0]?.body ?? {})).not.toContain("bot_ids");
    expect(calls[0]?.body).toEqual({
      launch_vars: { review_tier: "audit", gate_enabled: "false" },
    });
  });

  it("sends the exact set when the caller means to replace it (the unbind)", async () => {
    await updateForgeRepoBots("t1", "i1", ["review-pr"]);

    expect(calls[0]?.body).toEqual({ bot_ids: ["review-pr"] });
  });
});

describe("updateForgeRepoBots — routing on the wire", () => {
  // The server's three-way rule reads the FIELD's presence, not its value
  // shape alone: absent keeps the stored block, null clears it, an object
  // replaces it wholesale. JSON.stringify drops undefined keys and KEEPS
  // null ones — exactly the mapping the rule needs, and exactly what a
  // body-cleaning refactor would silently break, so it is witnessed here
  // where the server's PATCH tests cannot see.
  it("omits routing entirely when the caller says nothing (keep)", async () => {
    await updateForgeRepoBots("t1", "i1", undefined);

    expect(Object.keys(calls[0]?.body ?? {})).not.toContain("routing");
  });

  it("sends routing: null when the caller clears the block", async () => {
    await updateForgeRepoBots("t1", "i1", undefined, undefined, undefined, null);

    expect(Object.keys(calls[0]?.body ?? {})).toContain("routing");
    expect(calls[0]?.body).toEqual({ routing: null });
  });

  it("sends the parsed object when the caller replaces the block", async () => {
    await updateForgeRepoBots("t1", "i1", undefined, undefined, undefined, {
      providers: ["anthropic"],
    });

    expect(calls[0]?.body).toEqual({ routing: { providers: ["anthropic"] } });
  });
});
