import { afterEach, describe, expect, it, vi } from "vitest";

import { FeatureUnavailableError, getAdminCredentialUsage } from "./adminCredUsage";

afterEach(() => {
  vi.unstubAllGlobals();
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const emptyList = {
  month: "2026-09",
  scope: {},
  credentials: [],
  metered_usd: 0,
  estimated_usd: 0,
};

describe("getAdminCredentialUsage", () => {
  it("GETs the platform view with no query when none is given", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(emptyList));
    vi.stubGlobal("fetch", fetchMock);

    await getAdminCredentialUsage();

    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/admin/credentials/usage");
    expect(init?.method ?? "GET").toBe("GET");
  });

  it("builds the tier/month query", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(emptyList));
    vi.stubGlobal("fetch", fetchMock);

    await getAdminCredentialUsage({ tier: "org", month: "2026-08" });

    const [path] = fetchMock.mock.calls[0] as [string];
    const url = new URL(path, "https://test.invalid");
    expect(url.pathname).toBe("/api/admin/credentials/usage");
    expect(url.searchParams.get("tier")).toBe("org");
    expect(url.searchParams.get("month")).toBe("2026-08");
  });

  it("forwards fingerprint (exclusive of repo) as-is", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(emptyList));
    vi.stubGlobal("fetch", fetchMock);

    await getAdminCredentialUsage({ fingerprint: "abc123" });

    const [path] = fetchMock.mock.calls[0] as [string];
    const url = new URL(path, "https://test.invalid");
    expect(url.searchParams.get("fingerprint")).toBe("abc123");
    expect(url.searchParams.has("repo")).toBe(false);
  });

  it("maps a 404 to FeatureUnavailableError (counter not wired)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ error: "not enabled" }, 404));
    vi.stubGlobal("fetch", fetchMock);

    await expect(getAdminCredentialUsage()).rejects.toBeInstanceOf(FeatureUnavailableError);
  });
});
