import { afterEach, describe, expect, it, vi } from "vitest";

import { FeatureUnavailableError, clearUsageReadings } from "./adminUsageReadings";

afterEach(() => {
  vi.unstubAllGlobals();
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("clearUsageReadings", () => {
  it("DELETEs the fingerprint's readings and returns the deleted count", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse({ fingerprint: "e4ecd228", deleted: 3 }));
    vi.stubGlobal("fetch", fetchMock);

    const res = await clearUsageReadings("e4ecd228");

    expect(res).toEqual({ fingerprint: "e4ecd228", deleted: 3 });
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/admin/usage-readings/e4ecd228");
    expect(init.method).toBe("DELETE");
  });

  it("URL-encodes the fingerprint", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse({ fingerprint: "a/b", deleted: 0 }));
    vi.stubGlobal("fetch", fetchMock);

    await clearUsageReadings("a/b");

    const [path] = fetchMock.mock.calls[0] as [string];
    expect(path).toBe("/api/admin/usage-readings/a%2Fb");
  });

  it("maps a 404 to FeatureUnavailableError", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ error: "nope" }, 404));
    vi.stubGlobal("fetch", fetchMock);

    await expect(clearUsageReadings("x")).rejects.toBeInstanceOf(FeatureUnavailableError);
  });
});
