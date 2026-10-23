// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  apiRequest,
  cancelScheduledRefresh,
  REFRESH_RETRY_DELAYS_MS,
  setUnauthorizedHandler,
  SessionRenewalUnavailableError,
  ApiError,
  SESSION_RENEWED_AT_KEY,
} from "@/api/client";

// The silent refresh an expired access token triggers, driven through the
// real apiRequest with only the HTTP boundary replaced. What the refresh
// answers decides the session's fate: only a refusal (401/403) may sign the
// browser out — the sign-out handler POSTs /auth/logout, which REVOKES the
// refresh token. A refresh the server could not serve (5xx, rate limit, no
// answer) must leave the session alone: it is still valid, and revoking it
// over a blip signs a user out of a session nothing was wrong with.

// Each test is a browser that renewed its session long before (a renewal
// stamp older than any request here), so a request's 401 is current evidence
// and the refresh is presented on it directly; the stamp lives in
// localStorage, which the tests of this file share.
beforeEach(() => {
  localStorage.clear();
  localStorage.setItem(SESSION_RENEWED_AT_KEY, "-60000");
});

afterEach(() => {
  cancelScheduledRefresh();
  localStorage.clear();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  setUnauthorizedHandler(null);
});

// stub answers /api/probe with 401 until a refresh succeeded, and
// /api/auth/refresh with `refresh` (a status, or "network" for no answer).
function stub(refresh: number | "network") {
  const calls: string[] = [];
  let renewed = false;
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/api/auth/refresh") {
      if (refresh === "network") throw new TypeError("Failed to fetch");
      if (refresh >= 200 && refresh < 300) renewed = true;
      return new Response(JSON.stringify(refresh < 300 ? { user: { id: "u1" } } : { error: "identity unavailable" }), { status: refresh });
    }
    if (path === "/api/probe") {
      return renewed
        ? new Response(JSON.stringify({ ok: true }), { status: 200 })
        : new Response(JSON.stringify({ error: "token expired" }), { status: 401 });
    }
    throw new Error(`unexpected request ${path}`);
  }));
  return calls;
}

describe("the silent refresh of an expired access token", () => {
  for (const status of [500, 502, 503, 429] as const) {
    it(`answered ${status}: the request fails as transient and nothing signs the session out`, async () => {
      const calls = stub(status);
      let signedOut = 0;
      setUnauthorizedHandler(() => { signedOut++; });
      const err = await apiRequest("/api/probe").then(
        () => null,
        (e: unknown) => e,
      );
      expect(err).toBeInstanceOf(SessionRenewalUnavailableError);
      expect((err as SessionRenewalUnavailableError).refreshStatus).toBe(status);
      expect((err as ApiError).status).toBe(503);
      expect(signedOut).toBe(0);
      expect(calls).toEqual(["GET /api/probe", "POST /api/auth/refresh"]);
    });
  }

  it("got no answer: the request fails as transient and nothing signs the session out", async () => {
    stub("network");
    let signedOut = 0;
    setUnauthorizedHandler(() => { signedOut++; });
    const err = await apiRequest("/api/probe").then(() => null, (e: unknown) => e);
    expect(err).toBeInstanceOf(SessionRenewalUnavailableError);
    expect((err as SessionRenewalUnavailableError).refreshStatus).toBeNull();
    expect(signedOut).toBe(0);
  });

  for (const status of [401, 403] as const) {
    it(`answered ${status}: the session is over and the sign-out handler runs once`, async () => {
      stub(status);
      let signedOut = 0;
      setUnauthorizedHandler(() => { signedOut++; });
      const err = await apiRequest("/api/probe").then(() => null, (e: unknown) => e);
      expect(err).toBeInstanceOf(ApiError);
      expect(err).not.toBeInstanceOf(SessionRenewalUnavailableError);
      expect((err as ApiError).status).toBe(401);
      expect(signedOut).toBe(1);
    });
  }

  it("answered 200: the request is replayed with the renewed session", async () => {
    const calls = stub(200);
    let signedOut = 0;
    setUnauthorizedHandler(() => { signedOut++; });
    await expect(apiRequest("/api/probe")).resolves.toEqual({ ok: true });
    expect(signedOut).toBe(0);
    expect(calls).toEqual(["GET /api/probe", "POST /api/auth/refresh", "GET /api/probe"]);
  });

  it("is presented once for requests that expire together", async () => {
    const calls = stub(200);
    await Promise.all([apiRequest("/api/probe"), apiRequest("/api/probe"), apiRequest("/api/probe")]);
    expect(calls.filter((c) => c === "POST /api/auth/refresh")).toHaveLength(1);
  });
});

// stubSequence answers /api/probe with 401 and /api/auth/refresh with the
// statuses of seq in turn, the last one repeating.
function stubSequence(...seq: number[]) {
  const calls: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/api/auth/refresh") {
      const status = (seq.length > 1 ? seq.shift() : seq[0]) ?? 500;
      return new Response(JSON.stringify(status < 300 ? { user: { id: "u1" } } : { error: "identity unavailable" }), { status });
    }
    if (path === "/api/probe") return new Response(JSON.stringify({ error: "token expired" }), { status: 401 });
    throw new Error(`unexpected request ${path}`);
  }));
  return () => calls.filter((c) => c === "POST /api/auth/refresh").length;
}

// The reuse window the server answers a second presentation of a rotated
// token in (auth.DefaultRefreshReuseGrace).
const SERVER_REUSE_WINDOW_MS = 30_000;

describe("a renewal the server could not serve", () => {
  it("is presented again on its own, inside the server's window, until the server answers", async () => {
    vi.useFakeTimers();
    const refreshes = stubSequence(500, 500, 200);
    let signedOut = 0;
    setUnauthorizedHandler(() => { signedOut++; });
    await expect(apiRequest("/api/probe")).rejects.toBeInstanceOf(SessionRenewalUnavailableError);
    expect(refreshes()).toBe(1);
    await vi.advanceTimersByTimeAsync(REFRESH_RETRY_DELAYS_MS[0] ?? 0);
    expect(refreshes()).toBe(2);
    await vi.advanceTimersByTimeAsync(REFRESH_RETRY_DELAYS_MS[1] ?? 0);
    expect(refreshes()).toBe(3);
    // Renewed: the series is over.
    await vi.advanceTimersByTimeAsync(60_000);
    expect(refreshes()).toBe(3);
    expect(signedOut).toBe(0);
  });

  it("stops after its last delay, every attempt inside the server's window", async () => {
    vi.useFakeTimers();
    const refreshes = stubSequence(503);
    await expect(apiRequest("/api/probe")).rejects.toBeInstanceOf(SessionRenewalUnavailableError);
    const series = REFRESH_RETRY_DELAYS_MS.reduce((a, b) => a + b, 0);
    expect(series).toBeLessThan(SERVER_REUSE_WINDOW_MS);
    await vi.advanceTimersByTimeAsync(series);
    expect(refreshes()).toBe(1 + REFRESH_RETRY_DELAYS_MS.length);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(refreshes()).toBe(1 + REFRESH_RETRY_DELAYS_MS.length);
  });

  it("ends the session when the re-presentation is refused", async () => {
    vi.useFakeTimers();
    const refreshes = stubSequence(500, 401);
    let signedOut = 0;
    setUnauthorizedHandler(() => { signedOut++; });
    await expect(apiRequest("/api/probe")).rejects.toBeInstanceOf(SessionRenewalUnavailableError);
    expect(signedOut).toBe(0);
    await vi.advanceTimersByTimeAsync(REFRESH_RETRY_DELAYS_MS[0] ?? 0);
    expect(refreshes()).toBe(2);
    expect(signedOut).toBe(1);
  });
});
