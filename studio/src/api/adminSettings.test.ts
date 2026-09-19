import { afterEach, describe, expect, it, vi } from "vitest";

import {
  FeatureUnavailableError,
  getBotRoles,
  getBotVars,
  getPlatformCredentialsSettings,
  getSandboxSettings,
  getUsageCaps,
  putBotRoles,
  putBotVars,
  putPlatformCredentialsSettings,
  putSandboxSettings,
  putUsageCaps,
} from "./adminSettings";

afterEach(() => {
  vi.unstubAllGlobals();
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function call(mock: ReturnType<typeof vi.fn>, i = 0): [string, RequestInit] {
  return mock.mock.calls[i] as [string, RequestInit];
}

describe("adminSettings — reads", () => {
  it("GETs each family at its /api/admin/settings/* path", async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ origin: "db" })));
    vi.stubGlobal("fetch", fetchMock);

    await getUsageCaps();
    await getBotRoles();
    await getSandboxSettings();
    await getBotVars();
    await getPlatformCredentialsSettings();

    expect(fetchMock.mock.calls.map((c) => (c as unknown as [string])[0])).toEqual([
      "/api/admin/settings/usage-caps",
      "/api/admin/settings/bot-roles",
      "/api/admin/settings/sandbox",
      "/api/admin/settings/bot-vars",
      "/api/admin/settings/platform-credentials",
    ]);
    for (const [, init] of fetchMock.mock.calls as unknown as [string, RequestInit][]) {
      expect(init?.method ?? "GET").toBe("GET");
    }
  });
});

describe("adminSettings — writes send only the given keys (merge semantics)", () => {
  it("PUT usage-caps forwards exactly the patch, incl. an explicit null", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ source: "db" }));
    vi.stubGlobal("fetch", fetchMock);

    await putUsageCaps({ five_hour_pct: 80, week_pct: null });

    const [path, init] = call(fetchMock);
    expect(path).toBe("/api/admin/settings/usage-caps");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(String(init.body))).toEqual({ five_hour_pct: 80, week_pct: null });
  });

  it("PUT usage-caps omits an unspecified window entirely", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ source: "db" }));
    vi.stubGlobal("fetch", fetchMock);

    await putUsageCaps({ five_hour_pct: 50 });

    const [, init] = call(fetchMock);
    const body = JSON.parse(String(init.body));
    expect(body).toEqual({ five_hour_pct: 50 });
    expect("week_pct" in body).toBe(false);
  });

  it("PUT bot-roles / sandbox / bot-vars / platform-credentials hit their paths with PUT", async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ origin: "db" })));
    vi.stubGlobal("fetch", fetchMock);

    await putBotRoles({ reviewer: "review-pr", brancher: null });
    await putSandboxSettings({ default_image: null });
    await putBotVars({ ITERION_FOO: "bar", ITERION_OLD: null });
    await putPlatformCredentialsSettings({ enforce: true, orgs: ["org-1"] });

    const calls = fetchMock.mock.calls as unknown as [string, RequestInit][];
    expect(calls.map(([p]) => p)).toEqual([
      "/api/admin/settings/bot-roles",
      "/api/admin/settings/sandbox",
      "/api/admin/settings/bot-vars",
      "/api/admin/settings/platform-credentials",
    ]);
    expect(calls.map(([, i]) => i.method)).toEqual(["PUT", "PUT", "PUT", "PUT"]);
    expect(JSON.parse(String(calls[0]![1].body))).toEqual({ reviewer: "review-pr", brancher: null });
    expect(JSON.parse(String(calls[2]![1].body))).toEqual({ ITERION_FOO: "bar", ITERION_OLD: null });
  });
});

describe("adminSettings — feature gate", () => {
  it("maps a 404 to FeatureUnavailableError (store not wired)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ error: "not found" }, 404),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(getUsageCaps()).rejects.toBeInstanceOf(FeatureUnavailableError);
  });

  it("rethrows a non-404 error unchanged", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ error: "boom" }, 500),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(getBotRoles()).rejects.not.toBeInstanceOf(FeatureUnavailableError);
  });
});
