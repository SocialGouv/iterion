import { describe, expect, it, vi } from "vitest";

import { buildWsUrlWith, type BuildWsUrlDeps } from "./wsUrl";

function baseDeps(over: Partial<BuildWsUrlDeps> = {}): BuildWsUrlDeps {
  return {
    baseUrl: "/api",
    isDesktop: () => false,
    isWailsHosted: () => false,
    getDesktopWsBase: vi.fn().mockResolvedValue(null),
    locationProtocol: "http:",
    locationHost: "localhost:4891",
    ...over,
  };
}

describe("buildWsUrlWith", () => {
  it("derives a relative ws:// url from window.location on http", async () => {
    const url = await buildWsUrlWith(baseDeps(), "/ws/runs/abc");
    expect(url).toBe("ws://localhost:4891/api/ws/runs/abc");
  });

  it("upgrades to wss:// when the page is https", async () => {
    const url = await buildWsUrlWith(
      baseDeps({ locationProtocol: "https:", locationHost: "studio.example.com" }),
      "/ws",
    );
    expect(url).toBe("wss://studio.example.com/api/ws");
  });

  it("rewrites http:// in an absolute BASE_URL to ws://", async () => {
    const url = await buildWsUrlWith(
      baseDeps({ baseUrl: "http://localhost:4891/api" }),
      "/ws/runs/abc",
    );
    expect(url).toBe("ws://localhost:4891/api/ws/runs/abc");
  });

  it("rewrites https:// in an absolute BASE_URL to wss://", async () => {
    const url = await buildWsUrlWith(
      baseDeps({ baseUrl: "https://studio.example.com/api" }),
      "/ws",
    );
    expect(url).toBe("wss://studio.example.com/api/ws");
  });

  it("uses getDesktopWsBase when running inside the Wails wrapper", async () => {
    const getDesktopWsBase = vi
      .fn()
      .mockResolvedValue("ws://127.0.0.1:54321/api/ws/runs/abc?t=tok");
    const url = await buildWsUrlWith(
      baseDeps({ isDesktop: () => true, getDesktopWsBase }),
      "/ws/runs/abc",
    );
    expect(getDesktopWsBase).toHaveBeenCalledWith("/api/ws/runs/abc");
    expect(url).toBe("ws://127.0.0.1:54321/api/ws/runs/abc?t=tok");
  });

  it("falls through to window.location when isDesktop but bindings returned null", async () => {
    // Desktop bridge unavailable mid-startup. We must still produce something
    // the caller can dial — falling through to window.location is safe in
    // browser-hosted dev mode; in a Wails-hosted page isWailsHosted() flips
    // true and the next case applies.
    const url = await buildWsUrlWith(
      baseDeps({
        isDesktop: () => true,
        getDesktopWsBase: vi.fn().mockResolvedValue(null),
      }),
      "/ws",
    );
    expect(url).toBe("ws://localhost:4891/api/ws");
  });

  it("throws a transient when Wails-hosted but bindings aren't ready", async () => {
    await expect(
      buildWsUrlWith(
        baseDeps({
          isDesktop: () => true,
          isWailsHosted: () => true,
          getDesktopWsBase: vi.fn().mockResolvedValue(null),
        }),
        "/ws",
      ),
    ).rejects.toThrow(/desktop bindings not ready/);
  });
});
