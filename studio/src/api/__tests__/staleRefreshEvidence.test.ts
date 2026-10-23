// @vitest-environment jsdom
// A request's 401 is evidence about the access cookie it was SENT with, not
// about the one the browser holds when the 401 arrives: another tab may have
// renewed the session in between. Presenting the refresh token on such stale
// evidence rotates the successor that tab just received — which turns any
// presentation of the previous token still in flight into reuse, and the
// server revokes the lineage: every tab of the browser is signed out. So a
// request whose 401 predates a renewal replays instead of refreshing: the
// renewal stamp in localStorage tells it, and where there is no stamp to
// read, the cookie the browser holds now (GET /auth/me) does — asked again of
// the stamp once that read answered, since the read is evidence of the same
// kind.
//
// Several tabs are several instances of the real client module over one
// cookie jar and one localStorage. The server is modelled on pkg/auth: a live
// token presented rotates; a rotated token presented within 30 s whose
// successor is live and unrotated is answered that successor; any other
// presentation of a rotated token is reuse — the lineage is revoked. Each
// request is held until the test releases it, so the order is chosen.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SESSION_RENEWED_AT_KEY } from "@/api/client";

type Client = typeof import("@/api/client");
interface Row { rotatedAt?: number; successor?: string; revoked: boolean }
// release(true): the server processes the request, and its answer is lost on
// the way back — the client sees a network error, the jar does not move.
interface Held { tab: number; method: string; path: string; sent: { refresh: string | null; access: string | null }; release: (lose?: boolean) => void }

let tokens: Map<string, Row>;
let live: Set<string>;
let jar: { refresh: string | null; access: string | null };
let n: number;
let log: string[];
let held: Held[];
let presentedBy: number[];
let reuse: boolean;
// The tab whose module is calling fetch: set around each tab's call.
let actingTab: number;

beforeEach(() => {
  localStorage.clear();
  tokens = new Map([["T0", { revoked: false }]]);
  live = new Set();
  jar = { refresh: "T0", access: "A0" };
  n = 0;
  log = [];
  held = [];
  presentedBy = [];
  reuse = false;
  actingTab = 0;
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    const sent = { ...jar };
    const tab = actingTab;
    log.push(`tab ${tab} sends ${method} ${path} [refresh=${sent.refresh} access=${sent.access}]`);
    const lose = await new Promise<boolean | undefined>((resolve) => held.push({ tab, method, path, sent, release: resolve }));
    let status = 200;
    let cookie: { refresh: string | null; access: string | null } | null = null;
    if (path === "/api/auth/refresh") {
      presentedBy.push(tab);
      const row = sent.refresh ? tokens.get(sent.refresh) : undefined;
      if (!row || row.revoked) { status = 401; cookie = { refresh: null, access: null }; }
      else if (row.rotatedAt === undefined) {
        n++; row.rotatedAt = Date.now(); row.successor = `T${n}`; tokens.set(`T${n}`, { revoked: false }); live.add(`A${n}`);
        cookie = { refresh: `T${n}`, access: `A${n}` };
        log.push(`  rotated ${sent.refresh} -> T${n}`);
      } else {
        const succ = tokens.get(row.successor!);
        if (Date.now() - row.rotatedAt <= 30_000 && succ && !succ.revoked && succ.rotatedAt === undefined) {
          n++; live.add(`A${n}`); cookie = { refresh: row.successor!, access: `A${n}` };
          log.push(`  redelivered ${row.successor}`);
        } else {
          for (const r of tokens.values()) r.revoked = true;
          reuse = true;
          status = 401; cookie = { refresh: null, access: null };
          log.push(`  REUSE of ${sent.refresh}: the lineage is revoked`);
        }
      }
    } else if (path === "/api/auth/me" || path === "/api/probe") {
      status = sent.access && live.has(sent.access) ? 200 : 401;
      log.push(`  ${path} [${sent.access}] -> ${status}`);
    }
    if (lose) {
      log.push(`  the answer to tab ${tab}'s ${method} ${path} is lost`);
      throw new TypeError("network error: the answer was lost");
    }
    if (cookie) jar = cookie;
    return new Response("{}", { status });
  }));
});

afterEach(() => {
  for (const h of held.splice(0)) h.release();
  vi.unstubAllGlobals();
  localStorage.clear();
});

async function openTabs(count: number): Promise<Client[]> {
  const tabs: Client[] = [];
  for (let i = 0; i < count; i++) {
    vi.resetModules();
    tabs.push(await import("@/api/client"));
  }
  return tabs;
}

const settle = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); await new Promise((r) => setTimeout(r, 5)); };

// request makes tab `tab` send GET /api/probe; every fetch its module makes
// for it until the next settle() is attributed to that tab.
function request(tabs: Client[], tab: number): Promise<string> {
  actingTab = tab;
  return tabs[tab - 1]!.apiRequest("/api/probe").then(() => "ok", (e: unknown) => (e as Error).name);
}

// next waits for the held request matching what, and takes it off the list.
async function next(what: { tab: number; method?: string; path: string }): Promise<Held> {
  for (let i = 0; i < 200; i++) {
    const at = held.findIndex((h) => h.tab === what.tab && h.path === what.path && (!what.method || h.method === what.method));
    if (at >= 0) return held.splice(at, 1)[0]!;
    await new Promise((r) => setTimeout(r, 2));
  }
  throw new Error(`no held ${what.method ?? ""} ${what.path} from tab ${what.tab}; held: ${held.map((h) => `${h.tab}:${h.method} ${h.path}`).join(", ")}\n${log.join("\n")}`);
}

async function release(h: Held, tabForWhatFollows: number, lose = false) {
  actingTab = tabForWhatFollows;
  h.release(lose);
  await settle();
}

describe("a request whose 401 predates another tab's renewal", () => {
  it("replays on the renewed cookie instead of rotating the successor (renewal stamp)", async () => {
    // This browser renewed its session long before: the stamp is older than
    // every request here.
    localStorage.setItem(SESSION_RENEWED_AT_KEY, String(Date.now() - 60_000));
    const tabs = await openTabs(3);
    // Tab 1's request 401s (the access token expired); it presents T0, and
    // the server is slow with that presentation.
    void request(tabs, 1);
    await release(await next({ tab: 1, path: "/api/probe" }), 1);
    const tab1Refresh = await next({ tab: 1, method: "POST", path: "/api/auth/refresh" });
    // Tab 2 sends a request with the expired access cookie: slow too.
    const tab2 = request(tabs, 2);
    const tab2Probe = await next({ tab: 2, path: "/api/probe" });
    // Tab 3's request 401s and its refresh is served at once: T0 -> T1.
    const tab3 = request(tabs, 3);
    await release(await next({ tab: 3, path: "/api/probe" }), 3);
    await release(await next({ tab: 3, method: "POST", path: "/api/auth/refresh" }), 3);
    await release(await next({ tab: 3, path: "/api/probe" }), 3);
    expect(await tab3).toBe("ok");
    // Tab 2's request, sent before that renewal, answers 401 now.
    await release(tab2Probe, 2);
    await release(await next({ tab: 2, path: "/api/probe" }), 2);
    expect(await tab2).toBe("ok");
    // Tab 1's presentation of T0 is processed last: inside the window, T1
    // still unrotated — redelivered, not reuse.
    await release(tab1Refresh, 1);
    for (const h of held.splice(0)) await release(h, h.tab);
    expect(reuse).toBe(false);
    expect(presentedBy.filter((t) => t === 2)).toEqual([]);
    expect(log.filter((l) => l.includes("rotated"))).toEqual(["  rotated T0 -> T1"]);
  });

  it("checks the cookie it holds now when there is no stamp to read", async () => {
    // Storage the browser refuses: no tab can record or read a renewal.
    vi.stubGlobal("localStorage", undefined);
    const tabs = await openTabs(3);
    void request(tabs, 1);
    await release(await next({ tab: 1, path: "/api/probe" }), 1);
    // No stamp: tab 1 reads the cookie it holds (still A0: 401), then presents.
    await release(await next({ tab: 1, path: "/api/auth/me" }), 1);
    const tab1Refresh = await next({ tab: 1, method: "POST", path: "/api/auth/refresh" });
    const tab2 = request(tabs, 2);
    const tab2Probe = await next({ tab: 2, path: "/api/probe" });
    const tab3 = request(tabs, 3);
    await release(await next({ tab: 3, path: "/api/probe" }), 3);
    await release(await next({ tab: 3, path: "/api/auth/me" }), 3);
    await release(await next({ tab: 3, method: "POST", path: "/api/auth/refresh" }), 3);
    await release(await next({ tab: 3, path: "/api/probe" }), 3);
    expect(await tab3).toBe("ok");
    // Tab 2's stale 401: the cookie it holds now is A1, live — it replays.
    await release(tab2Probe, 2);
    await release(await next({ tab: 2, path: "/api/auth/me" }), 2);
    await release(await next({ tab: 2, path: "/api/probe" }), 2);
    expect(await tab2).toBe("ok");
    await release(tab1Refresh, 1);
    for (const h of held.splice(0)) await release(h, h.tab);
    expect(reuse).toBe(false);
    expect(presentedBy.filter((t) => t === 2)).toEqual([]);
  });

  it("re-presents nothing when its own presentation, sent before another tab's renewal, lost its answer", async () => {
    localStorage.setItem(SESSION_RENEWED_AT_KEY, String(Date.now() - 60_000));
    const tabs = await openTabs(2);
    // Tab 1's request 401s and presents T0; the server is slow with it.
    void request(tabs, 1);
    await release(await next({ tab: 1, path: "/api/probe" }), 1);
    const tab1Refresh = await next({ tab: 1, method: "POST", path: "/api/auth/refresh" });
    // Tab 2 renews meanwhile: T0 -> T1, stamped.
    const tab2 = request(tabs, 2);
    await release(await next({ tab: 2, path: "/api/probe" }), 2);
    await release(await next({ tab: 2, method: "POST", path: "/api/auth/refresh" }), 2);
    await release(await next({ tab: 2, path: "/api/probe" }), 2);
    expect(await tab2).toBe("ok");
    // Tab 1's T0 is processed now — redelivered T1 — and its answer is lost:
    // tab 1 sees a network error and arms its series.
    await release(tab1Refresh, 1, true);
    // The series' first attempt (2 s), past any read it could hold (1.5 s):
    // what tab 1 presented was the token the jar held when it sent it, and the
    // session was renewed since — nothing to re-present.
    await new Promise((r) => setTimeout(r, 4000));
    for (const h of held.splice(0)) await release(h, h.tab);
    // Each tab presented once (in the order the server processed them): no re-presentation.
    expect(presentedBy).toEqual([2, 1]);
    expect(log.filter((l) => l.includes("rotated"))).toEqual(["  rotated T0 -> T1"]);
    expect(reuse).toBe(false);
  }, 15000);

  it("asks the stamp again once its own read of the cookie answered: a renewal while that read was out supersedes it", async () => {
    // A browser that never renewed: no stamp yet.
    const tabs = await openTabs(3);
    // Tab 1's request 401s; with no stamp it reads the cookie — a read sent
    // with A0, slow.
    const tab1 = request(tabs, 1);
    await release(await next({ tab: 1, path: "/api/probe" }), 1);
    const tab1Read = await next({ tab: 1, path: "/api/auth/me" });
    // Tab 2 presents T0, slowly: a presentation in flight.
    void request(tabs, 2);
    await release(await next({ tab: 2, path: "/api/probe" }), 2);
    await release(await next({ tab: 2, path: "/api/auth/me" }), 2);
    const tab2Refresh = await next({ tab: 2, method: "POST", path: "/api/auth/refresh" });
    // Tab 3 renews: T0 -> T1, the stamp is written.
    const tab3 = request(tabs, 3);
    await release(await next({ tab: 3, path: "/api/probe" }), 3);
    await release(await next({ tab: 3, path: "/api/auth/me" }), 3);
    await release(await next({ tab: 3, method: "POST", path: "/api/auth/refresh" }), 3);
    await release(await next({ tab: 3, path: "/api/probe" }), 3);
    expect(await tab3).toBe("ok");
    // Tab 1's read answers 401 — about A0, the cookie it was sent with.
    await release(tab1Read, 1);
    await release(await next({ tab: 1, path: "/api/probe" }), 1);
    expect(await tab1).toBe("ok");
    await release(tab2Refresh, 2);
    for (const h of held.splice(0)) await release(h, h.tab);
    expect(reuse).toBe(false);
    expect(presentedBy.filter((t) => t === 1)).toEqual([]);
  });
});
