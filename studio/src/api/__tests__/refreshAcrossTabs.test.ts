// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SESSION_RENEWED_AT_KEY } from "@/api/client";

// Several tabs of one browser are several copies of the client module — own
// refreshInFlight, own retry series, own timers — over ONE cookie jar. The
// refresh token in that jar is shared, and the server retires it on every
// rotation: a tab presenting a token another tab has already rotated is reuse,
// which the server answers by revoking every session of the account. So the
// end of one outage must cost ONE rotation, however many tabs armed a series
// during it and in whatever order their timers fire.
//
// The refresh endpoint is modelled on the server: a live token presented
// rotates (the jar moves to the successor and the access cookie is renewed), a
// store outage answers 503 and rotates nothing, and GET /auth/me is the
// authenticated read — the access cookie, no refresh token, nothing rotated.

type Client = typeof import("@/api/client");

const SERVER_REUSE_WINDOW_MS = 30_000;

interface Jar {
  /** The generation of the refresh token the cookie jar holds. */
  refresh: number;
  /** Whether the access cookie is one the server still accepts. */
  accessLive: boolean;
  storeDown: boolean;
  presented: string[];
  rotations: string[];
  reads: string[];
}

function newJar(overrides: Partial<Jar> = {}): Jar {
  return { refresh: 0, accessLive: false, storeDown: false, presented: [], rotations: [], reads: [], ...overrides };
}

function stubServer(jar: Jar, options: { readNeverAnswers?: boolean } = {}) {
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/auth/me" && options.readNeverAnswers) {
      jar.reads.push(`@${Date.now()}`);
      // What the platform does with an aborted fetch: reject. Nothing else
      // ever answers this read.
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
      });
    }
    if (path === "/api/auth/refresh") {
      jar.presented.push(`T${jar.refresh}@${Date.now()}`);
      if (jar.storeDown) return json({ error: "identity unavailable" }, 503);
      jar.rotations.push(`T${jar.refresh}->T${jar.refresh + 1}`);
      jar.refresh++;
      jar.accessLive = true;
      return json({ user: { id: "u1" } });
    }
    if (path === "/api/auth/me") {
      jar.reads.push(`@${Date.now()}`);
      if (jar.storeDown) return json({ error: "identity unavailable" }, 503);
      return jar.accessLive ? json({ user: { id: "u1" }, orgs: [] }) : json({ error: "authentication required" }, 401);
    }
    if (path === "/api/probe") {
      return jar.accessLive ? json({ ok: true }) : json({ error: "token expired" }, 401);
    }
    throw new Error(`unexpected ${path}`);
  }));
}

let tabs: Client[] = [];

async function openTabs(n: number): Promise<Client[]> {
  tabs = [];
  for (let i = 0; i < n; i++) {
    vi.resetModules();
    tabs.push(await import("@/api/client"));
  }
  return tabs;
}

// The renewal stamp the tabs compare their evidence with lives in
// localStorage, which every test of this file shares, as the tabs of one
// browser do. Each test starts from a browser that renewed its session long
// before (a stamp older than any request here): a request then presents on its
// own 401, as it does in a browser with any history. The stamp-less browser has
// its own witnesses (staleRefreshEvidence.test.ts).
beforeEach(() => {
  localStorage.clear();
  localStorage.setItem(SESSION_RENEWED_AT_KEY, "-60000");
});

afterEach(() => {
  for (const tab of tabs) tab.cancelScheduledRefresh();
  tabs = [];
  localStorage.clear();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("an outage that ends, with several tabs open", () => {
  it("costs one rotation, not one per tab", async () => {
    vi.useFakeTimers({ now: 0 });
    const jar = newJar({ storeDown: true });
    stubServer(jar);
    const open = await openTabs(3);
    // Each tab's next request after the access token expired, spread over a
    // second — all answered 503 by the refresh, all arming their own series.
    for (const [i, tab] of open.entries()) {
      await vi.advanceTimersByTimeAsync(i === 0 ? 0 : 500);
      await tab.apiRequest("/api/probe").catch((e: unknown) => e);
    }
    expect(jar.presented).toEqual(["T0@0", "T0@500", "T0@1000"]);
    // The store recovers, and nobody clicks anything for 30 s: the only thing
    // left is the three series' timers, firing at 2000, 2500 and 3000.
    await vi.advanceTimersByTimeAsync(400);
    jar.storeDown = false;
    await vi.advanceTimersByTimeAsync(30_000);
    expect(jar.rotations).toEqual(["T0->T1"]);
    // The two tabs whose timers fired after the renewal present nothing: the
    // stamp the renewing tab wrote says their evidence predates it.
    expect(jar.presented).toEqual(["T0@0", "T0@500", "T0@1000", "T0@2000"]);
  });

  it("renews once when the tabs act one at a time instead (no series armed)", async () => {
    vi.useFakeTimers({ now: 0 });
    const jar = newJar({ storeDown: true });
    stubServer(jar);
    const open = await openTabs(3);
    for (const [i, tab] of open.entries()) {
      await vi.advanceTimersByTimeAsync(i === 0 ? 0 : 500);
      await tab.apiRequest("/api/probe").catch((e: unknown) => e);
    }
    // Disarm every series: what is left is three users clicking, a second
    // apart, after the store recovered.
    for (const tab of open) tab.cancelScheduledRefresh();
    await vi.advanceTimersByTimeAsync(400);
    jar.storeDown = false;
    for (const tab of open) {
      await vi.advanceTimersByTimeAsync(1000);
      await tab.apiRequest("/api/probe").catch((e: unknown) => e);
    }
    // The first request renews; the other two ride the renewed access cookie.
    expect(jar.rotations).toEqual(["T0->T1"]);
  });

  it("presents nothing when the session was renewed while the series waited", async () => {
    vi.useFakeTimers({ now: 0 });
    const jar = newJar({ storeDown: true });
    stubServer(jar);
    const [tab] = await openTabs(1);
    await tab!.apiRequest("/api/probe").catch((e: unknown) => e);
    expect(jar.presented).toEqual(["T0@0"]);
    // Another tab renews the session 1.5 s in — the jar moves to the
    // successor, and the token this series kept is retired.
    await vi.advanceTimersByTimeAsync(1500);
    jar.storeDown = false;
    jar.refresh++;
    jar.accessLive = true;
    jar.rotations.push("T0->T1 (another tab)");
    await vi.advanceTimersByTimeAsync(30_000);
    expect(jar.reads).toEqual(["@2000"]);
    expect(jar.presented).toEqual(["T0@0"]);
    expect(jar.rotations).toEqual(["T0->T1 (another tab)"]);
  });
});

describe("the renewals a single tab is owed", () => {
  it("renews within the series after a 5xx, inside the server's reuse window", async () => {
    vi.useFakeTimers({ now: 0 });
    const jar = newJar({ storeDown: true });
    stubServer(jar);
    const [tab] = await openTabs(1);
    await tab!.apiRequest("/api/probe").catch((e: unknown) => e);
    await vi.advanceTimersByTimeAsync(1400);
    jar.storeDown = false;
    await vi.advanceTimersByTimeAsync(30_000);
    // The renewal is not lost: the series presented again, and the read it
    // runs first confirmed the access cookie was still dead.
    expect(jar.rotations).toEqual(["T0->T1"]);
    expect(jar.presented).toEqual(["T0@0", "T0@2000"]);
    const renewedAt = Number(jar.presented[1]!.split("@")[1]);
    expect(renewedAt).toBeLessThan(SERVER_REUSE_WINDOW_MS);
    // The request that follows rides the renewed session.
    await expect(tab!.apiRequest("/api/probe")).resolves.toEqual({ ok: true });
  });

  it("still re-presents when the read before it never answers", async () => {
    vi.useFakeTimers({ now: 0 });
    const jar = newJar({ storeDown: true });
    stubServer(jar, { readNeverAnswers: true });
    const [tab] = await openTabs(1);
    await tab!.apiRequest("/api/probe").catch((e: unknown) => e);
    await vi.advanceTimersByTimeAsync(1400);
    jar.storeDown = false;
    await vi.advanceTimersByTimeAsync(30_000);
    // The read is bounded: it cannot swallow the renewal the series owes.
    expect(jar.reads[0]).toBe("@2000");
    expect(jar.rotations).toEqual(["T0->T1"]);
    const renewedAt = Number(jar.presented[1]!.split("@")[1]);
    expect(renewedAt).toBe(3500);
  });

  it("refreshes on the request itself when its access token expired, with no read first", async () => {
    const jar = newJar();
    stubServer(jar);
    const [tab] = await openTabs(1);
    await expect(tab!.apiRequest("/api/probe")).resolves.toEqual({ ok: true });
    expect(jar.rotations).toEqual(["T0->T1"]);
    // The 401 is current evidence — the last renewal the stamp records is older
    // than the request — so the request path reads nothing before presenting.
    expect(jar.reads).toEqual([]);
  });
});
