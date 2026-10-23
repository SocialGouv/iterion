// @vitest-environment jsdom
// What the previous account's requests do once its sign-out began. A refresh
// the sign-out gives up waiting for (its 2 s settle) is aborted: its answer —
// a 401 that clears the cookies, or a late rotation that sets the previous
// session's — must not land on the account that signs in next. And a request
// sent for the previous session renews nothing and signs nobody out when its
// 401 arrives after the next account signed in: what it would renew, or sign
// out, is that account's session.
//
// Real AuthProvider and client; the HTTP boundary keeps a cookie jar read when
// a request is sent and moved when its answer arrives, and drops the answer of
// an aborted request, as a browser does.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { apiRequest, cancelScheduledRefresh, setUnauthorizedHandler } from "@/api/client";

const ALICE = { id: "alice", email: "alice@example.test", status: "active", is_super_admin: false };
const BOB = { id: "bob", email: "bob@example.test", status: "active", is_super_admin: false };
const ORGS = [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }];

let jar: { refresh: string | null; access: string | null };
let accounts: Map<string, typeof ALICE>;
let calls: string[];
let releaseRefresh: ((status: 200 | 401) => void) | null;
let refreshAborted: boolean;
let probeHold: boolean;
let releaseProbe: (() => void) | null;
let meHold: boolean;
let releaseMe: (() => void) | null;
let unauthorized: number;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}|{a.user?.email ?? "-"}</span>
      <button onClick={() => void a.signOut()}>sign out</button>
      <button onClick={() => void a.signIn("bob@example.test", "pw")}>sign in as bob</button>
    </div>
  );
}

// Wire registers the sign-out handler the shell's AuthGate registers.
function Wire() {
  const a = useAuth();
  setUnauthorizedHandler(() => { unauthorized++; void a.signOut(); });
  return null;
}

beforeEach(() => {
  localStorage.clear();
  jar = { refresh: "TA0", access: "alice-A0" };
  accounts = new Map([["alice-A0", ALICE]]);
  calls = [];
  releaseRefresh = null;
  refreshAborted = false;
  probeHold = false;
  releaseProbe = null;
  meHold = false;
  releaseMe = null;
  unauthorized = 0;
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    const sent = { ...jar };
    calls.push(`${method} ${path} [refresh=${sent.refresh ?? "none"} access=${sent.access ?? "none"}]`);
    const who = sent.access ? accounts.get(sent.access) : undefined;
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") {
      const answer = () => (who ? json({ user: who, orgs: ORGS, active_org_id: "o1", active_team_id: "t1", active_role: "member" }) : json({ error: "authentication required" }, 401));
      if (!meHold) return answer();
      return new Promise<Response>((resolve) => { releaseMe = () => resolve(answer()); });
    }
    if (path === "/api/probe") {
      // The server reads the access token when it processes the request.
      const answer = () => {
        const reader = sent.access ? accounts.get(sent.access) : undefined;
        return reader ? json({ ok: true, as: reader.email }) : json({ error: "token expired" }, 401);
      };
      if (!probeHold) return answer();
      return new Promise<Response>((resolve) => { releaseProbe = () => resolve(answer()); });
    }
    if (path === "/api/auth/logout") {
      jar = { refresh: null, access: null };
      return new Response(null, { status: 204 });
    }
    if (path === "/api/auth/login") {
      jar = { refresh: "TB0", access: "bob-B0" };
      accounts.set("bob-B0", BOB);
      return json({ user: BOB });
    }
    if (path === "/api/auth/refresh") {
      if (sent.refresh !== "TA0") return json({ error: "auth: session revoked" }, 401);
      return new Promise<Response>((resolve, reject) => {
        init?.signal?.addEventListener("abort", () => {
          refreshAborted = true;
          reject(new DOMException("aborted", "AbortError"));
        });
        releaseRefresh = (status) => {
          if (refreshAborted) return;
          if (status === 401) {
            jar = { refresh: null, access: null };
            resolve(json({ error: "auth: session revoked" }, 401));
            return;
          }
          jar = { refresh: "TA1", access: "alice-A1" };
          accounts.set("alice-A1", ALICE);
          resolve(json({ user: ALICE }));
        };
      });
    }
    throw new Error(`unexpected ${method} ${path}`);
  }));
});

afterEach(() => {
  releaseRefresh?.(401);
  releaseProbe?.();
  releaseMe?.();
  cancelScheduledRefresh();
  setUnauthorizedHandler(null);
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

const state = () => screen.getByTestId("state").textContent;

describe("the previous account's refresh, in flight past the sign-out's settle", () => {
  for (const late of [401, 200] as const) {
    it(`is aborted: its late ${late} reaches neither the cookies nor the account signed in next`, async () => {
      render(<AuthProvider><Probe /><Wire /></AuthProvider>);
      await screen.findByText("authenticated|alice@example.test");
      // Alice's access token expires; a request of her view 401s and its
      // refresh hangs on a slow store.
      accounts.delete("alice-A0");
      const aliceRequest = apiRequest("/api/probe").then(() => "resolved", (e: unknown) => (e as Error).name);
      await waitFor(() => expect(releaseRefresh).not.toBeNull());
      await act(async () => { screen.getByText("sign out").click(); });
      await screen.findByText("anonymous|-", {}, { timeout: 5000 });
      expect(refreshAborted).toBe(true);
      await act(async () => { screen.getByText("sign in as bob").click(); });
      await screen.findByText("authenticated|bob@example.test");
      // Alice's refresh answers now — dropped, as the browser drops an aborted
      // request's answer.
      await act(async () => { releaseRefresh!(late); releaseRefresh = null; await new Promise((r) => setTimeout(r, 200)); });
      await aliceRequest;
      expect(state()).toBe("authenticated|bob@example.test");
      expect(jar).toEqual({ refresh: "TB0", access: "bob-B0" });
      expect(unauthorized).toBe(0);
    }, 15000);
  }
});

describe("a request whose refresh answers after its sign-out began", () => {
  it("is not replayed: the session it renewed is the one the user is leaving", async () => {
    render(<AuthProvider><Probe /><Wire /></AuthProvider>);
    await screen.findByText("authenticated|alice@example.test");
    accounts.delete("alice-A0");
    const aliceRequest = apiRequest("/api/probe").then(() => "resolved", (e: unknown) => (e as Error).name);
    await waitFor(() => expect(releaseRefresh).not.toBeNull());
    await act(async () => { screen.getByText("sign out").click(); });
    // The refresh answers inside the sign-out's settle: renewed.
    const before = calls.length;
    await act(async () => { releaseRefresh!(200); releaseRefresh = null; await new Promise((r) => setTimeout(r, 100)); });
    await screen.findByText("anonymous|-");
    expect(await aliceRequest).toBe("ApiError");
    expect(calls.slice(before).filter((c) => c.startsWith("GET /api/probe"))).toEqual([]);
    expect(calls.slice(before)).toEqual(["POST /api/auth/logout [refresh=TA1 access=alice-A1]"]);
    expect(unauthorized).toBe(0);
  });
});

describe("a request whose read of the cookie answers after its sign-out", () => {
  it("presents nothing for the session the user left", async () => {
    render(<AuthProvider><Probe /><Wire /></AuthProvider>);
    await screen.findByText("authenticated|alice@example.test");
    // Alice's access token expired; her request 401s, and with no renewal
    // stamp to compare with it reads the cookie the browser holds — slowly.
    accounts.delete("alice-A0");
    meHold = true;
    const aliceRequest = apiRequest("/api/probe").then(() => "resolved", (e: unknown) => (e as Error).name);
    await waitFor(() => expect(releaseMe).not.toBeNull());
    meHold = false;
    await act(async () => { screen.getByText("sign out").click(); });
    await screen.findByText("anonymous|-");
    // The sign-out is over when the read answers 401.
    const before = calls.length;
    await act(async () => { releaseMe!(); releaseMe = null; await new Promise((r) => setTimeout(r, 100)); });
    expect(await aliceRequest).toBe("ApiError");
    expect(calls.slice(before)).toEqual([]);
    expect(unauthorized).toBe(0);
  });
});

describe("a request of a session signed out whose 401 arrives after the sign-out", () => {
  it("reads and presents nothing: the session it would renew is over", async () => {
    render(<AuthProvider><Probe /><Wire /></AuthProvider>);
    await screen.findByText("authenticated|alice@example.test");
    probeHold = true;
    const aliceRequest = apiRequest("/api/probe").then(() => "resolved", (e: unknown) => (e as Error).name);
    await waitFor(() => expect(releaseProbe).not.toBeNull());
    accounts.delete("alice-A0");
    await act(async () => { screen.getByText("sign out").click(); });
    await screen.findByText("anonymous|-");
    const before = calls.length;
    await act(async () => { releaseProbe!(); releaseProbe = null; await new Promise((r) => setTimeout(r, 200)); });
    expect(await aliceRequest).toBe("ApiError");
    expect(calls.slice(before)).toEqual([]);
    expect(unauthorized).toBe(0);
  });
});

describe("a request of the previous session whose 401 arrives after the next sign-in", () => {
  it("renews nothing and signs nobody out", async () => {
    render(<AuthProvider><Probe /><Wire /></AuthProvider>);
    await screen.findByText("authenticated|alice@example.test");
    // Alice's request is on the wire, slow; her access token is past its
    // expiry by the time the server reads it.
    probeHold = true;
    const aliceRequest = apiRequest("/api/probe").then(() => "resolved", (e: unknown) => (e as Error).name);
    await waitFor(() => expect(releaseProbe).not.toBeNull());
    accounts.delete("alice-A0");
    await act(async () => { screen.getByText("sign out").click(); });
    await screen.findByText("anonymous|-");
    await act(async () => { screen.getByText("sign in as bob").click(); });
    await screen.findByText("authenticated|bob@example.test");
    const before = calls.length;
    await act(async () => { releaseProbe!(); releaseProbe = null; await new Promise((r) => setTimeout(r, 200)); });
    expect(await aliceRequest).toBe("ApiError");
    // Nothing was presented with Bob's refresh token, and Bob is still in.
    expect(calls.slice(before)).toEqual([]);
    expect(unauthorized).toBe(0);
    expect(state()).toBe("authenticated|bob@example.test");
    expect(jar).toEqual({ refresh: "TB0", access: "bob-B0" });
  });
});
