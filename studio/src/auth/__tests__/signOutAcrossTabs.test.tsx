// @vitest-environment jsdom
// A sign-out is pending in localStorage from its start until a logout answers
// 2xx, and every tab of the browser reads that mark: a tab that still shows
// the account follows the sign-out, a tab that loads presents the logout
// before reading any identity, no tab renews the session meanwhile or adopts
// an identity read that answers after the mark appeared, and the warning lifts
// in every tab once a logout confirms it.
//
// Real AuthProvider and client; another tab is the mark it writes and the
// storage event the browser delivers for it.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { useActiveRepoStore } from "@/store/activeRepo";
import {
  apiRequest,
  cancelScheduledRefresh,
  SESSION_RENEWED_AT_KEY,
  SessionRenewalUnavailableError,
  SIGN_OUT_PENDING_KEY,
} from "@/api/client";

const USER = { id: "u1", email: "u1@example.test", status: "active", is_super_admin: false };
const ONE_ORG = [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" }] }];
const TWO_ORGS = [...ONE_ORG, { org_id: "o2", org_name: "O2", org_slug: "o2", org_role: "owner", teams: [] }];

let jar: { refresh: string | null; access: boolean };
let calls: string[];
let tree: typeof ONE_ORG;
let meHold: boolean;
let releaseMe: (() => void) | null;
let logoutStatus: number;
let logoutHold: boolean;
let releaseLogout: (() => void) | null;
let refreshStatus: number;
let probeHold: boolean;
let heldProbes: (() => void)[];

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}|orgs={a.orgs.map((o) => o.org_id).join(",")}|unconfirmed={String(a.signOutUnconfirmed)}</span>
      <button onClick={() => void a.signOut()}>sign out</button>
      <button onClick={() => void a.reloadIdentity()}>reload</button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  jar = { refresh: "T0", access: true };
  calls = [];
  tree = ONE_ORG;
  meHold = false;
  releaseMe = null;
  logoutStatus = 204;
  logoutHold = false;
  releaseLogout = null;
  refreshStatus = 200;
  probeHold = false;
  heldProbes = [];
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    calls.push(`${method} ${path}`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") {
      const answer = () => (jar.access ? json({ user: USER, orgs: tree, active_org_id: "o1", active_team_id: "ta", active_role: "member" }) : json({ error: "authentication required" }, 401));
      if (!meHold) return answer();
      return new Promise<Response>((resolve) => { releaseMe = () => resolve(answer()); });
    }
    if (path === "/api/auth/logout") {
      const answer = () => {
        if (logoutStatus !== 204) return json({ error: "identity unavailable" }, logoutStatus);
        jar = { refresh: null, access: false };
        return new Response(null, { status: 204 });
      };
      if (!logoutHold) return answer();
      return new Promise<Response>((resolve) => { releaseLogout = () => resolve(answer()); });
    }
    if (path === "/api/auth/refresh") {
      if (refreshStatus !== 200) return json({ error: "identity unavailable" }, refreshStatus);
      jar.access = true;
      return json({ user: USER });
    }
    if (path === "/api/probe") {
      const answer = () => (jar.access ? json({ ok: true }) : json({ error: "token expired" }, 401));
      if (!probeHold) return answer();
      return new Promise<Response>((resolve) => { heldProbes.push(() => resolve(answer())); });
    }
    throw new Error(`unexpected ${method} ${path}`);
  }));
});

afterEach(() => {
  releaseMe?.();
  releaseLogout?.();
  for (const r of heldProbes.splice(0)) r();
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

const state = () => screen.getByTestId("state").textContent;

// anotherTabSignsOut writes the mark the way another tab's sign-out does, and
// delivers the storage event the browser fires in this tab for it.
function anotherTabSignsOut() {
  const at = String(Date.now());
  localStorage.setItem(SIGN_OUT_PENDING_KEY, at);
  window.dispatchEvent(new StorageEvent("storage", { key: SIGN_OUT_PENDING_KEY, newValue: at }));
}

function anotherTabConfirms() {
  localStorage.removeItem(SIGN_OUT_PENDING_KEY);
  window.dispatchEvent(new StorageEvent("storage", { key: SIGN_OUT_PENDING_KEY, newValue: null }));
}

describe("another tab's sign-out", () => {
  it("signs this tab out too, with its own logout", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|/);
    await act(async () => { anotherTabSignsOut(); });
    await screen.findByText("anonymous|orgs=|unconfirmed=false");
    expect(calls.filter((c) => c === "POST /api/auth/logout")).toHaveLength(1);
    expect(localStorage.getItem(SIGN_OUT_PENDING_KEY)).toBeNull();
  });

  it("is settled before a tab that loads reads any identity", async () => {
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    logoutStatus = 500;
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("anonymous|orgs=|unconfirmed=true");
    expect(calls).toEqual(["GET /api/server/info", "POST /api/auth/logout"]);
  });

  it("is pending from the moment it starts, for every tab and the next load to see", async () => {
    logoutHold = true;
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|/);
    await act(async () => { screen.getByText("sign out").click(); });
    await waitFor(() => expect(releaseLogout).not.toBeNull());
    // The logout is on the wire: a tab closed now, or a logout that never
    // answers, still leaves the mark.
    expect(localStorage.getItem(SIGN_OUT_PENDING_KEY)).not.toBeNull();
    await act(async () => { releaseLogout!(); releaseLogout = null; });
    await screen.findByText("anonymous|orgs=|unconfirmed=false");
    expect(localStorage.getItem(SIGN_OUT_PENDING_KEY)).toBeNull();
  });

  it("starts no identity read in any tab while it is pending", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|/);
    // The mark is there; this tab's storage event has not been delivered yet.
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    const before = calls.length;
    await act(async () => { screen.getByText("reload").click(); await new Promise((r) => setTimeout(r, 50)); });
    expect(calls.slice(before)).toEqual([]);
  });

  it("leaves the session unrenewed in every tab while it is pending", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|/);
    // The mark is there; this tab's storage event has not been delivered yet.
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    jar.access = false;
    const err = await apiRequest("/api/probe").then(() => null, (e: unknown) => e);
    expect(err).toBeInstanceOf(SessionRenewalUnavailableError);
    expect(calls.filter((c) => c === "POST /api/auth/refresh")).toEqual([]);
  });

  it("stops a retry series armed before it: nothing is read or presented once it is pending", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|/);
    // A request's renewal the server could not serve arms the series (2 s).
    jar.access = false;
    refreshStatus = 503;
    await apiRequest("/api/probe").catch(() => undefined);
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    const before = calls.length;
    await new Promise((r) => setTimeout(r, 2600));
    expect(calls.slice(before)).toEqual([]);
  });

  it("presents nothing for a request replayed on another tab's renewal whose 401 arrives once it is pending", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|/);
    jar.access = false;
    probeHold = true;
    const request = apiRequest("/api/probe").then(() => null, (e: unknown) => e);
    await waitFor(() => expect(heldProbes).toHaveLength(1));
    // Another tab renews after the request was sent: its 401 is stale, and it
    // is replayed rather than refreshed.
    localStorage.setItem(SESSION_RENEWED_AT_KEY, String(Date.now()));
    heldProbes.shift()!();
    await waitFor(() => expect(heldProbes).toHaveLength(1));
    // A sign-out is pending by the time the replay's 401 arrives.
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    probeHold = false;
    heldProbes.shift()!();
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.filter((c) => c === "POST /api/auth/refresh")).toEqual([]);
    expect(await request).toBeInstanceOf(SessionRenewalUnavailableError);
  });

  it("drops an identity read that answers after it began", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|orgs=o1|unconfirmed=false");
    meHold = true;
    tree = TWO_ORGS;
    await act(async () => { screen.getByText("reload").click(); });
    await waitFor(() => expect(releaseMe).not.toBeNull());
    // The mark appears while the read is out; this tab's storage event has
    // not been delivered yet.
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    await act(async () => { releaseMe!(); releaseMe = null; await new Promise((r) => setTimeout(r, 50)); });
    expect(state()).toBe("authenticated|orgs=o1|unconfirmed=false");
  });

  // The fact travels with the EVENT: the mark APPEARED, whether or not the
  // other tab's logout has answered and removed it since. A tab frozen or busy
  // for the one round trip that takes reads the current mark as "no sign-out"
  // and keeps the account on screen until its own next request 401s.
  it("leaves the account in a tab that processes its events only after the logout settled", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|orgs=o1|unconfirmed=false");
    useActiveRepoStore.setState({ storageKey: "iterion.activeRepo.u1.ta", selection: { kind: "repo", key: "gh/acme/app" } });
    // The other tab: mark, logout 204 (the server clears the shared cookies),
    // unmark. Then this tab processes the two events the browser queued.
    const at = String(Date.now());
    localStorage.setItem(SIGN_OUT_PENDING_KEY, at);
    jar = { refresh: null, access: false };
    localStorage.removeItem(SIGN_OUT_PENDING_KEY);
    const before = calls.length;
    await act(async () => {
      window.dispatchEvent(new StorageEvent("storage", { key: SIGN_OUT_PENDING_KEY, newValue: at }));
      window.dispatchEvent(new StorageEvent("storage", { key: SIGN_OUT_PENDING_KEY, oldValue: at, newValue: null }));
    });
    await screen.findByText("anonymous|orgs=|unconfirmed=false");
    // Locally: the account goes, per-account context included.
    expect(useActiveRepoStore.getState().selection).toEqual({ kind: "none" });
    // And nothing is presented: that sign-out already answered, and a logout
    // of this browser's — with no lineage to name — would be about whatever
    // session it opens next.
    expect(calls.slice(before)).toEqual([]);
  });

  it("lifts the warning in this tab once another tab's logout confirms it", async () => {
    logoutStatus = 500;
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|/);
    await act(async () => { screen.getByText("sign out").click(); });
    await screen.findByText("anonymous|orgs=|unconfirmed=true");
    await act(async () => { anotherTabConfirms(); });
    expect(state()).toBe("anonymous|orgs=|unconfirmed=false");
  });
});
