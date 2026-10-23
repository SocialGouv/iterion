// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { SignOutHeldError } from "@/api/signOutMachine";
import {
  apiRequest,
  cancelScheduledRefresh,
  SessionRenewalUnavailableError,
  setUnauthorizedHandler,
} from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

// "A signed-out browser presents nothing" is a window, not an instant: it
// opens when the user clicks and closes when /logout has answered. Three
// things live in it —
//   - a refresh already IN FLIGHT, whose answer would arm a new series and
//     re-present the token after the logout (and whose own 401 would sign out
//     whatever session the browser holds by then);
//   - a refresh that would START, because a request's access token happens to
//     expire while the logout travels;
//   - the cookies: a refresh that rotates while the logout is on the wire sets
//     the successor's cookies AFTER the logout cleared them, leaving a
//     signed-out browser holding a session's credential.
// And the window must CLOSE: a browser that stays "signing out" cannot renew
// any later session, so the next access expiry signs that one out too.

const identity = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};

// The browser's cookie jar, as the server moves it: a refresh that succeeds
// rotates the refresh cookie and mints a fresh access cookie, a logout clears
// both.
interface Jar { refresh: string | null; access: boolean }

let jar: Jar;
let calls: string[];
let rotations: number;
let signedOut: number;
// How /api/auth/refresh answers: "now" with a status, or held until the test
// releases it.
let refreshMode: { kind: "now"; status: number } | { kind: "held" };
let releaseRefresh: ((status: number) => void) | null;
// How /api/auth/logout answers: at once, held until released, or never
// reaching the server at all.
let logoutMode: "ok" | "held" | "network";
let releaseLogout: (() => void) | null;
// The promise the Probe's sign-out returned, so a test can await the whole
// sign-out and time it.
let signOutDone: Promise<void> | null;
// The promise the Probe's sign-in returned.
let signInDone: Promise<void> | null;

function Probe() {
  const a = useAuth();
  return (
    <div>
      <p data-testid="status">{a.status}</p>
      <button onClick={() => { signOutDone = a.signOut(); }}>sign out</button>
      <button onClick={() => { signInDone = a.signIn("next@example.test", "pw"); }}>sign in</button>
      <button onClick={() => { signInDone = a.signIn("next@example.test", "pw", { anyway: true }); }}>sign in anyway</button>
    </div>
  );
}

const refreshCalls = () => calls.filter((c) => c.startsWith("POST /api/auth/refresh"));
const logins = () => calls.filter((c) => c.startsWith("POST /api/auth/login"));
const logoutCalls = () => calls.filter((c) => c.startsWith("POST /api/auth/logout"));
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

beforeEach(() => {
  jar = { refresh: "T0", access: true };
  calls = [];
  rotations = 0;
  signedOut = 0;
  refreshMode = { kind: "now", status: 200 };
  releaseRefresh = null;
  logoutMode = "ok";
  releaseLogout = null;
  signOutDone = null;
  signInDone = null;
  // A sign-out the server did not confirm is marked in localStorage, which
  // the tests of this file share: each starts from a browser with none.
  localStorage.clear();
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path} [refresh=${jar.refresh ?? "none"}]`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") return jar.access ? json(identity) : json({ error: "authentication required" }, 401);
    if (path === "/api/probe") return jar.access ? json({ ok: true }) : json({ error: "token expired" }, 401);
    if (path === "/api/auth/login") {
      jar.refresh = "T0";
      jar.access = true;
      return json({ user: identity.user });
    }
    if (path === "/api/auth/logout") {
      if (logoutMode === "network") throw new TypeError("Failed to fetch");
      const clear = () => { jar.refresh = null; jar.access = false; return new Response(null, { status: 204 }); };
      if (logoutMode === "ok") return clear();
      return new Promise<Response>((resolve) => { releaseLogout = () => resolve(clear()); });
    }
    if (path === "/api/auth/refresh") {
      const answer = (status: number) => {
        if (status < 200 || status >= 300) return json({ error: "identity unavailable" }, status);
        rotations++;
        jar.refresh = `T${rotations}`;
        jar.access = true;
        return json({ user: identity.user });
      };
      if (refreshMode.kind === "now") return answer(refreshMode.status);
      return new Promise<Response>((resolve) => { releaseRefresh = (status) => resolve(answer(status)); });
    }
    throw new Error(`unexpected ${path}`);
  }));
});

afterEach(async () => {
  // Let go of anything still held so no promise of this test outlives it.
  releaseRefresh?.(503);
  releaseLogout?.();
  releaseRefresh = null;
  releaseLogout = null;
  await Promise.resolve();
  cancelScheduledRefresh();
  setUnauthorizedHandler(null);
  cleanup();
  vi.unstubAllGlobals();
});

// signedIn renders the provider and waits for the bootstrap to settle, then
// expires the access cookie so the next request 401s the way a 15-minute
// access token does.
async function signedInWithAnExpiredAccessToken() {
  render(<AuthProvider><Probe /></AuthProvider>);
  await screen.findByText("authenticated");
  setUnauthorizedHandler(() => { signedOut++; });
  jar.access = false;
}

describe("a refresh in flight when the user signs out", () => {
  it("presents nothing, and signs nothing out, after the sign-out", async () => {
    await signedInWithAnExpiredAccessToken();
    refreshMode = { kind: "held" };
    const req = apiRequest("/api/probe").catch((e: unknown) => e);
    await waitFor(() => expect(releaseRefresh).not.toBeNull());

    screen.getByText("sign out").click();
    // The refresh in flight answers 503 — the outcome that arms the series.
    releaseRefresh!(503);
    releaseRefresh = null;
    await act(async () => { await signOutDone; });
    await screen.findByText("anonymous");
    const logoutAt = calls.findIndex((c) => c.startsWith("POST /api/auth/logout"));
    expect(logoutAt).toBeGreaterThan(-1);
    await req;

    // Past the first delay of the series that answer would have armed.
    await sleep(2600);
    const after = calls.slice(logoutAt + 1);
    expect(after.filter((c) => c.startsWith("POST /api/auth/refresh"))).toEqual([]);
    expect(after.filter((c) => c.startsWith("POST /api/auth/logout"))).toEqual([]);
    expect(signedOut).toBe(0);
  }, 15000);

  it("lets it finish first, so the logout carries the token the browser ends on", async () => {
    await signedInWithAnExpiredAccessToken();
    refreshMode = { kind: "held" };
    const req = apiRequest("/api/probe").catch((e: unknown) => e);
    await waitFor(() => expect(releaseRefresh).not.toBeNull());

    screen.getByText("sign out").click();
    await waitFor(() => expect(signOutDone).not.toBeNull());
    await sleep(50);
    // The logout is not on the wire yet: the sign-out is waiting for the
    // refresh it found in flight.
    expect(logoutCalls()).toEqual([]);

    // That refresh succeeds: the jar moves to the successor.
    releaseRefresh!(200);
    releaseRefresh = null;
    await act(async () => { await signOutDone; });
    await req;

    // The logout presented the successor — the token the browser actually
    // holds — and nothing set a session's cookies after it cleared them.
    expect(logoutCalls()).toEqual(["POST /api/auth/logout [refresh=T1]"]);
    expect(jar).toEqual({ refresh: null, access: false });
    await sleep(100);
    expect(jar).toEqual({ refresh: null, access: false });
  }, 15000);

  it("does not wait for one that never answers", async () => {
    await signedInWithAnExpiredAccessToken();
    refreshMode = { kind: "held" };
    // Never released: this is the refresh that does not answer. afterEach lets
    // it go, so awaiting it here would be awaiting the thing under test.
    void apiRequest("/api/probe").catch(() => undefined);
    await waitFor(() => expect(releaseRefresh).not.toBeNull());

    const started = Date.now();
    screen.getByText("sign out").click();
    await waitFor(() => expect(signOutDone).not.toBeNull());
    const outcome = await act(async () =>
      Promise.race([
        signOutDone!.then(() => "signed out"),
        sleep(6000).then(() => "still waiting for the refresh"),
      ]),
    );
    const elapsed = Date.now() - started;
    expect(outcome).toBe("signed out");
    // It waited for the refresh, and gave up on it.
    expect(elapsed).toBeGreaterThanOrEqual(1500);
    await screen.findByText("anonymous");
    expect(logoutCalls()).toHaveLength(1);
  }, 15000);
});

describe("a request whose access token expires while the sign-out is in flight", () => {
  it("presents nothing and signs nothing out", async () => {
    await signedInWithAnExpiredAccessToken();
    logoutMode = "held";
    screen.getByText("sign out").click();
    await waitFor(() => expect(logoutCalls()).toHaveLength(1));
    expect(refreshCalls()).toEqual([]);

    // A request of some other view 401s now, between the logout's send and
    // its answer.
    const err = await apiRequest("/api/probe").then(() => null, (e: unknown) => e);
    expect(err).toBeInstanceOf(SessionRenewalUnavailableError);
    expect(refreshCalls()).toEqual([]);
    expect(signedOut).toBe(0);

    releaseLogout!();
    releaseLogout = null;
    await act(async () => { await signOutDone; });
    await screen.findByText("anonymous");
    expect(refreshCalls()).toEqual([]);
  }, 15000);
});

describe("the window closes when the sign-out returns", () => {
  for (const logout of ["ok", "network"] as const) {
    it(`renews the next session's access token after a sign-out whose logout ${logout === "ok" ? "answered" : "never reached the server"}`, async () => {
      await signedInWithAnExpiredAccessToken();
      logoutMode = logout;
      screen.getByText("sign out").click();
      await act(async () => { await signOutDone; });
      await screen.findByText("anonymous");

      // A later session on this browser — another account signs in — whose
      // access token then expires. A "signing out" flag the sign-out failed to
      // clear would make this renewal impossible, and the 401 it cannot renew
      // signs that session out too. Under the machine, a sign-in over a
      // sign-out whose logout never reached the server is HELD first (the
      // record stands and this browser holds the named session, I4): the
      // first attempt throws the hold and sends no login, and the explicit
      // "sign in anyway" — which moves the record to ABANDONED, its notice
      // staying (I2) — is what lets the session land. A logout that ANSWERED
      // cleared the record, so the sign-in is not held at all.
      if (logout === "network") {
        let held: unknown = null;
        await act(async () => {
          screen.getByText("sign in").click();
          held = await signInDone!.then(() => null, (e: unknown) => e);
        });
        expect(held).toBeInstanceOf(SignOutHeldError);
        expect(logins()).toHaveLength(0);
        await act(async () => {
          screen.getByText("sign in anyway").click();
          await signInDone;
        });
      } else {
        await act(async () => {
          screen.getByText("sign in").click();
          await signInDone;
        });
      }
      await screen.findByText("authenticated");
      const before = refreshCalls().length;
      jar.access = false;
      refreshMode = { kind: "now", status: 200 };
      await expect(apiRequest("/api/probe")).resolves.toEqual({ ok: true });
      expect(refreshCalls()).toHaveLength(before + 1);
      expect(signedOut).toBe(0);
    }, 15000);
  }
});
