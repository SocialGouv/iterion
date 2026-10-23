// @vitest-environment jsdom
// The background re-read of a stale tree (markTreeStale) over long incidents:
// it backs off to one attempt a minute instead of polling every 3 s for as
// long as the incident lasts; it is ONE loop however many committed steps
// fail their read; it renews the session when the access token aged out
// during the incident, through the client's guarded refresh, and ends with
// the session when the server refuses that renewal; and it stops when the
// provider unmounts.
//
// Real AuthProvider and client, fetch at the HTTP boundary, fake timers once
// the provider is signed in. An attempt is one identity read — up to three
// GETs 0.5 s and 1 s apart (fetchIdentity's own retries) — so attempts are
// told apart by the gap before their first GET.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import { AuthProvider, IDENTITY_RETRY_INTERVAL_MS, TREE_REREAD_CAP_MS, TREE_REREAD_FACTOR, useAuth } from "@/auth/AuthContext";
import { backoffDelay, type Backoff } from "@/auth/backoffLoop";
import { cancelScheduledRefresh } from "@/api/client";

const ALICE = { id: "u-alice", email: "alice@example.test", status: "active", is_super_admin: false };
const ONE_ORG = [{ org_id: "o1", org_name: "Org one", org_slug: "org-one", org_role: "admin", teams: [{ team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" }] }];
const TWO_ORGS = [...ONE_ORG, { org_id: "o2", org_name: "Org two", org_slug: "org-two", org_role: "owner", teams: [] }];

let meFails: boolean;
let accessLive: boolean;
let refreshStatus: number;
let refreshes: number;
let reads: number[];
let tree: typeof ONE_ORG;

const BACKOFF: Backoff = { firstMs: IDENTITY_RETRY_INTERVAL_MS, factor: TREE_REREAD_FACTOR, capMs: TREE_REREAD_CAP_MS };

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}|orgs={a.orgs.map((o) => o.org_id).join(",")}|stale={String(a.treeStale)}</span>
      <button onClick={() => void a.reloadIdentity()}>reload</button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  meFails = false;
  accessLive = true;
  refreshStatus = 200;
  refreshes = 0;
  reads = [];
  tree = ONE_ORG;
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") {
      reads.push(Date.now());
      if (!accessLive) return json({ error: "token expired" }, 401);
      if (meFails) return json({ error: "identity unavailable" }, 500);
      return json({ user: ALICE, orgs: tree, active_org_id: "o1", active_org_role: "admin", active_team_id: "ta", active_role: "member" });
    }
    if (path === "/api/auth/refresh") {
      refreshes++;
      if (refreshStatus !== 200) return json({ error: "auth: session revoked" }, refreshStatus);
      accessLive = true;
      return json({ user: ALICE });
    }
    throw new Error(`unexpected ${init?.method ?? "GET"} ${path}`);
  }));
});

afterEach(() => {
  vi.useRealTimers();
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

const state = () => screen.getByTestId("state").textContent;
const advance = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });

// attemptStarts groups the reads from `from` on into attempts: a read more
// than 2 s after the previous one starts a new attempt.
function attemptStarts(from: number): number[] {
  const starts: number[] = [];
  let last = -Infinity;
  for (const t of reads.filter((r) => r >= from)) {
    if (t - last > 2000) starts.push(t);
    last = t;
  }
  return starts;
}

async function signedInThenFakeTime() {
  render(<AuthProvider><Probe /></AuthProvider>);
  await screen.findByText(/^authenticated\|orgs=o1\|stale=false/);
  vi.useFakeTimers({ now: 0 });
  reads = [];
}

// A committed step's re-read fails (the identity store is down): the tree is
// marked stale, and the loop starts.
async function staleAfterAFailedReload() {
  meFails = true;
  tree = TWO_ORGS;
  await act(async () => {
    screen.getByText("reload").click();
    await vi.advanceTimersByTimeAsync(2000);
  });
  expect(state()).toBe("authenticated|orgs=o1|stale=true");
}

describe("the stale tree's re-read", () => {
  it("backs off to one attempt a minute over a long incident", async () => {
    await signedInThenFakeTime();
    await staleAfterAFailedReload();
    const loopFrom = Date.now();
    await advance(60 * 60 * 1000);
    const starts = attemptStarts(loopFrom);
    const gaps = starts.slice(1).map((t, i) => t - starts[i]!);
    // Each gap is the attempt's own read (1.5 s for three failing GETs) plus
    // the loop's delay, which grows from 3 s by 1.5x to a minute.
    // (Fake timers keep whole milliseconds: a fractional delay lands within 1 ms.)
    const expected = gaps.map((_, i) => 1500 + backoffDelay(BACKOFF, i + 1));
    expect(Math.max(...gaps.map((g, i) => Math.abs(g - expected[i]!)))).toBeLessThanOrEqual(1);
    expect(gaps.length).toBeGreaterThan(50);
    expect(Math.max(...gaps)).toBe(1500 + TREE_REREAD_CAP_MS);
    // Per minute: the incident's last minute costs less than its second.
    const perMinute = (from: number) => reads.filter((t) => t >= from && t < from + 60_000).length;
    expect(perMinute(59 * 60_000)).toBeLessThan(perMinute(60_000));
    expect(reads.length).toBeLessThan(200);
    expect(state()).toBe("authenticated|orgs=o1|stale=true");
  }, 30000);

  it("runs ONE loop however many committed steps fail their read", async () => {
    await signedInThenFakeTime();
    await staleAfterAFailedReload();
    // Committed steps (a rename, an accepted invitation, a team created) that
    // fail their own read, at different phases of the loop's cycle — several
    // while one of its reads is in flight.
    for (let i = 0; i < 12; i++) {
      await act(async () => { screen.getByText("reload").click(); await vi.advanceTimersByTimeAsync(1700 + 370 * i); });
    }
    // Long after, the loop is at its cap: one attempt a minute, never two.
    await advance(20 * 60_000);
    const from = Date.now();
    await advance(10 * 60_000);
    const starts = attemptStarts(from);
    const gaps = starts.slice(1).map((t, i) => t - starts[i]!);
    expect(starts.length).toBeLessThanOrEqual(10);
    expect(Math.min(...gaps)).toBeGreaterThanOrEqual(TREE_REREAD_CAP_MS);
    // And once the server serves again, the one loop lands the tree and ends.
    meFails = false;
    await advance(2 * 60_000);
    expect(state()).toBe("authenticated|orgs=o1,o2|stale=false");
    const settled = reads.length;
    await advance(10 * 60_000);
    expect(reads.length).toBe(settled);
  }, 60000);

  it("renews the session through the guarded refresh when the access token aged out during the incident", async () => {
    await signedInThenFakeTime();
    await staleAfterAFailedReload();
    // 16 minutes of incident: the 15-minute access token ages out meanwhile.
    await advance(16 * 60 * 1000);
    accessLive = false;
    meFails = false;
    await advance(2 * 60_000);
    expect(refreshes).toBe(1);
    expect(state()).toBe("authenticated|orgs=o1,o2|stale=false");
  }, 30000);

  it("ends with the session when the server refuses its renewal", async () => {
    await signedInThenFakeTime();
    await staleAfterAFailedReload();
    accessLive = false;
    refreshStatus = 401;
    await advance(2 * 60_000);
    expect(refreshes).toBe(1);
    expect(state()).toBe("anonymous|orgs=|stale=false");
    const settled = reads.length;
    await advance(10 * 60_000);
    expect(reads.length).toBe(settled);
  }, 30000);

  it("stops when the provider unmounts while the loop waits for its next attempt", async () => {
    await signedInThenFakeTime();
    await staleAfterAFailedReload();
    cleanup();
    const atUnmount = reads.length;
    await vi.advanceTimersByTimeAsync(10 * 60 * 1000);
    expect(reads.length - atUnmount).toBe(0);
  }, 30000);

  it("starts no loop for a committed step whose read fails after the provider unmounted", async () => {
    await signedInThenFakeTime();
    meFails = true;
    // The step's read is in flight (three GETs over 1.5 s) when the provider
    // unmounts; it fails after.
    await act(async () => { screen.getByText("reload").click(); await vi.advanceTimersByTimeAsync(600); });
    cleanup();
    const atUnmount = reads.length;
    await vi.advanceTimersByTimeAsync(10 * 60 * 1000);
    expect(reads.length - atUnmount).toBeLessThanOrEqual(2);
  }, 30000);

  it("stops when the provider unmounts with a re-read in flight", async () => {
    await signedInThenFakeTime();
    await staleAfterAFailedReload();
    // The loop's first attempt fires: its read (three GETs over 1.5 s) is in
    // flight.
    const before = reads.length;
    while (reads.length === before) await advance(100);
    cleanup();
    const atUnmount = reads.length;
    await vi.advanceTimersByTimeAsync(10 * 60 * 1000);
    // The read in flight finishes its own retries; nothing is armed after it.
    expect(reads.length - atUnmount).toBeLessThanOrEqual(2);
  }, 30000);
});
