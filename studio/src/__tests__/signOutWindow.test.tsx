// @vitest-environment jsdom
// A sign-out is a WINDOW — from the click until the logout answered — and
// nothing may put the account back inside it: no identity load starts in it,
// none that answers after it is adopted, no refresh is presented. A logout
// with no 2xx is not a sign-out: the HttpOnly cookies are where they were, so
// the sign-out stays pending, the signed-out pages say so, and the logout is
// presented again before any identity is adopted — a reload included.
//
// The real App, AuthProvider, identity-unavailable screen and api client; only
// the destination views and the HTTP boundary are replaced. The boundary
// models the server and the browser: the cookie jar is read when a request is
// SENT and moved when its answer ARRIVES; an aborted request's answer never
// reaches the jar; access JWTs are stateless (valid until they expire,
// whatever happens to the refresh session); a refresh rotates the token it
// carries; a logout revokes the lineage of the token it carries — or, when its
// store fails, answers 500 and keeps the cookies.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useRoute } from "wouter";

vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: ({ signedIn }: { signedIn?: boolean }) => <h1>Public cloud home{signedIn ? " (signed in)" : ""}</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => <h1>Restricted account</h1> }));
vi.mock("@/views/ConfigShare", () => ({ default: () => <h1>Config share editor</h1> }));
vi.mock("@/views/Marketplace", () => ({ default: () => <h1>Public marketplace</h1> }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: function RequestedRun() {
  const [, params] = useRoute("/runs/:id");
  return <h1>Requested run {params?.id}</h1>;
} }));

import App from "@/App";
import { apiRequest, cancelScheduledRefresh, SessionRenewalUnavailableError } from "@/api/client";
import { stopLoop } from "@/api/signOutMachine";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const identity = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};

interface Jar { refresh: string | null; access: string | null }
interface Row { lineage: string; rotatedTo?: string; revoked: boolean }

let jar: Jar;
let liveAccess: Set<string>;
let rows: Map<string, Row>;
let n: number;
let meStoreDown: boolean;
let meHold: boolean;
let heldMe: (() => void)[];
let refreshHold: boolean;
let releaseRefresh: (() => void) | null;
let logoutMode: "ok" | "held" | "network" | "500";
let ssoProviders: { name: string; display: string }[];
let releaseLogout: (() => void) | null;
let log: string[];

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const shell = () => screen.queryByRole("heading", { name: /Requested run|Signed-in home/ });
const sentAfter = (i: number, what: string) => log.slice(i).filter((l) => l.startsWith(what));
// The warning the signed-out pages carry after a sign-out the server did not
// confirm, as the alert it is rendered as.
const unconfirmedAlert = () => screen.queryByText("Sign-out not confirmed")?.closest("[role=alert]") ?? null;

function rotate(tok: string): number {
  const row = rows.get(tok);
  if (!row || row.revoked || row.rotatedTo) return 401;
  n++;
  row.rotatedTo = `T${n}`;
  rows.set(`T${n}`, { lineage: row.lineage, revoked: false });
  liveAccess.add(`A${n}`);
  return 200;
}

function revokeLineageOf(tok: string | null) {
  const row = tok ? rows.get(tok) : undefined;
  if (!row) return;
  for (const r of rows.values()) if (r.lineage === row.lineage) r.revoked = true;
}

// answerWhen settles a request when `ready` runs, applying `effect` to the jar
// then — unless the request was aborted first: a browser drops an aborted
// request's answer, Set-Cookie included.
function answerWhen(signal: AbortSignal | null | undefined, hold: (ready: () => void) => void, effect: () => Response): Promise<Response> {
  return new Promise<Response>((resolve, reject) => {
    let settled = false;
    signal?.addEventListener("abort", () => {
      if (settled) return;
      settled = true;
      reject(new DOMException("aborted", "AbortError"));
    });
    hold(() => {
      if (settled) return;
      settled = true;
      resolve(effect());
    });
  });
}

beforeEach(() => {
  localStorage.clear();
  jar = { refresh: "T0", access: "A0" };
  liveAccess = new Set(["A0"]);
  rows = new Map([["T0", { lineage: "L0", revoked: false }]]);
  n = 0;
  meStoreDown = false;
  meHold = false;
  heldMe = [];
  refreshHold = false;
  releaseRefresh = null;
  logoutMode = "ok";
  releaseLogout = null;
  ssoProviders = [];
  log = [];
  window.history.replaceState({}, "", "/studio/runs/review-123");
  useServerInfoStore.setState({ info: { mode: "cloud", marketplace_enabled: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    const sent = { ...jar };
    log.push(`${method} ${path} [refresh=${sent.refresh ?? "none"} access=${sent.access ?? "none"}]`);
    const signal = init?.signal;
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud", marketplace_enabled: true });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: ssoProviders });
    if (path === "/api/auth/me") {
      const answer = () => {
        if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "authentication required" }, 401);
        if (meStoreDown) return json({ error: "identity unavailable" }, 500);
        return json(identity);
      };
      if (!meHold) return answer();
      return answerWhen(signal, (ready) => heldMe.push(ready), answer);
    }
    if (path === "/api/auth/refresh") {
      const answer = () => {
        const status = sent.refresh ? rotate(sent.refresh) : 401;
        jar = status === 200 ? { refresh: `T${n}`, access: `A${n}` } : { refresh: null, access: null };
        log.push(`  refresh ${sent.refresh ?? "none"} -> ${status === 200 ? jar.refresh : "401"}`);
        return status === 200 ? json({ user: identity.user }) : json({ error: "auth: session revoked" }, 401);
      };
      if (!refreshHold) return answer();
      return answerWhen(signal, (ready) => { releaseRefresh = ready; }, answer);
    }
    if (path === "/api/auth/login") {
      n++;
      rows.set(`T${n}`, { lineage: `L${n}`, revoked: false });
      liveAccess.add(`A${n}`);
      jar = { refresh: `T${n}`, access: `A${n}` };
      return json({ user: identity.user });
    }
    if (path === "/api/auth/logout") {
      if (logoutMode === "network") throw new TypeError("Failed to fetch");
      const answer = () => {
        if (logoutMode === "500") return json({ error: "identity unavailable" }, 500);
        revokeLineageOf(sent.refresh);
        jar = { refresh: null, access: null };
        log.push(`  logout: lineage of ${sent.refresh ?? "none"} revoked, jar cleared`);
        return new Response(null, { status: 204 });
      };
      if (logoutMode !== "held") return answer();
      return answerWhen(signal, (ready) => { releaseLogout = ready; }, answer);
    }
    if (path.startsWith("/api/")) {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "token expired" }, 401);
      return json({});
    }
    throw new Error(`unexpected ${method} ${path}`);
  }));
});

// React cleanup alone does not destroy module timers as a document unload does.
function unloadDocument() {
  cleanup();
  cancelScheduledRefresh();
  stopLoop();
}

afterEach(async () => {
  releaseLogout?.();
  releaseRefresh?.();
  for (const r of heldMe.splice(0)) r();
  await new Promise((r) => setTimeout(r, 10));
  unloadDocument();
  localStorage.clear();
  vi.unstubAllGlobals();
});

async function onTheIdentityUnavailableScreen() {
  meStoreDown = true;
  render(<App />);
  await screen.findByText("Your identity is unavailable", {}, { timeout: 6000 });
}

describe("the sign-out window", () => {
  it("starts no identity read and presents no refresh while the logout is on the wire", async () => {
    await onTheIdentityUnavailableScreen();
    // The store recovers and the access token ages out: the screen's next tick
    // would read the identity (401) and renew the session to read it again.
    meStoreDown = false;
    liveAccess.delete("A0");
    logoutMode = "held";
    screen.getByRole("button", { name: "Sign out" }).click();
    await waitFor(() => expect(sentAfter(0, "POST /api/auth/logout")).toHaveLength(1));
    const logoutAt = log.findIndex((l) => l.startsWith("POST /api/auth/logout"));
    // Past the screen's auto-retry cadence (3 s), the logout still on the wire.
    await new Promise((r) => setTimeout(r, 3600));
    expect(sentAfter(logoutAt + 1, "GET /api/auth/me")).toEqual([]);
    expect(sentAfter(logoutAt + 1, "POST /api/auth/refresh")).toEqual([]);
    expect(screen.getByText("Signing out…")).toBeTruthy();
    expect(screen.queryByText("Your identity is unavailable")).toBeNull();
    expect(shell()).toBeNull();
    releaseLogout!();
    releaseLogout = null;
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 6000 });
    expect(unconfirmedAlert()).toBeNull();
    expect(jar).toEqual({ refresh: null, access: null });
  }, 20000);

  it("drops an identity read sent before the click that answers after the logout", async () => {
    await onTheIdentityUnavailableScreen();
    // The store recovers; the screen's next tick reads the identity, slowly.
    meStoreDown = false;
    meHold = true;
    await waitFor(() => expect(heldMe.length).toBeGreaterThan(0), { timeout: 6000 });
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 6000 });
    // The read answers now: 200, for the access token the logout did not
    // reach (stateless, still within its 15 minutes).
    for (const r of heldMe.splice(0)) r();
    await new Promise((r) => setTimeout(r, 300));
    expect(shell()).toBeNull();
    expect(screen.getByRole("heading", { name: "Sign in to iterion" })).toBeTruthy();
  }, 20000);

  it("lets the bootstrap's renewal in flight finish first, so the logout carries the token the browser ends on", async () => {
    await onTheIdentityUnavailableScreen();
    // The store recovers and the access token ages out: the screen's next tick
    // reads the identity (401) and renews the session — slowly.
    meStoreDown = false;
    liveAccess.delete("A0");
    refreshHold = true;
    await waitFor(() => expect(releaseRefresh).not.toBeNull(), { timeout: 6000 });
    screen.getByRole("button", { name: "Sign out" }).click();
    await new Promise((r) => setTimeout(r, 300));
    // The renewal is the client's own presentation of the token, so the
    // sign-out waits for it instead of sending a logout it would overtake.
    expect(sentAfter(0, "POST /api/auth/logout")).toEqual([]);
    releaseRefresh!();
    releaseRefresh = null;
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 6000 });
    expect(sentAfter(0, "POST /api/auth/logout")).toEqual(["POST /api/auth/logout [refresh=T1 access=A1]"]);
    expect(jar).toEqual({ refresh: null, access: null });
    expect(rows.get("T1")?.revoked).toBe(true);
  }, 20000);
});

describe("a sign-out whose logout got no 2xx", () => {
  for (const failure of ["500", "network"] as const) {
    it(`says it is not confirmed and signs nobody back in on a reload (logout ${failure === "500" ? "answered 500" : "never reached the server"})`, async () => {
      await onTheIdentityUnavailableScreen();
      meStoreDown = false;
      logoutMode = failure;
      screen.getByRole("button", { name: "Sign out" }).click();
      await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 6000 });
      // Not a clean sign-in form: the browser still holds the session.
      expect(unconfirmedAlert()).not.toBeNull();
      expect(jar).toEqual({ refresh: "T0", access: "A0" });

      // A reload — or the next person at this computer.
      unloadDocument();
      const reloadAt = log.length;
      // The old document is gone while the reload is delayed past its first
      // automatic retry (2s). Its module timer must not send another logout.
      await new Promise((r) => setTimeout(r, 2100));
      render(<App />);
      await screen.findByText("Sign-out not confirmed", {}, { timeout: 6000 });
      expect(shell()).toBeNull();
      // The logout is presented again before anything else, and no identity is
      // read from the session it is meant to end.
      expect(log.slice(reloadAt).filter((l) => !l.startsWith("GET /api/auth/providers"))).toEqual([
        "GET /api/server/info [refresh=T0 access=A0]",
        "POST /api/auth/logout [refresh=T0 access=A0]",
      ]);
    }, 20000);
  }

  it("ends the session on the next load once the server answers, before reading any identity", async () => {
    await onTheIdentityUnavailableScreen();
    meStoreDown = false;
    logoutMode = "network";
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 6000 });

    logoutMode = "ok";
    unloadDocument();
    const reloadAt = log.length;
    render(<App />);
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 6000 });
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(jar).toEqual({ refresh: null, access: null });
    expect(sentAfter(reloadAt, "GET /api/auth/me")).toEqual([]);
  }, 20000);

  it("presents the logout again on Retry sign-out, and the warning lifts once it is confirmed", async () => {
    await onTheIdentityUnavailableScreen();
    meStoreDown = false;
    logoutMode = "500";
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 6000 });

    logoutMode = "ok";
    screen.getByRole("button", { name: "Retry sign-out" }).click();
    await waitFor(() => expect(screen.queryByText("Sign-out not confirmed")).toBeNull());
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(jar).toEqual({ refresh: null, access: null });
  }, 20000);

  it("ends within a bound when the logout never answers, and renews nothing meanwhile", async () => {
    await onTheIdentityUnavailableScreen();
    meStoreDown = false;
    logoutMode = "held"; // a hung connection: never released
    const clickAt = log.length;
    screen.getByRole("button", { name: "Sign out" }).click();
    // The record is written BEFORE the logout goes on the wire, so the
    // warning is up while the window is still open (the banner is pinned over
    // the "Signing out" screen); the window itself closes at the bound.
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    expect(shell()).toBeNull();
    // A request of this tab needs a renewal meanwhile: the session the user
    // asked to end is not renewed.
    liveAccess.delete("A0");
    const err = await apiRequest("/api/probe").then(() => null, (e: unknown) => e);
    expect(err).toBeInstanceOf(SessionRenewalUnavailableError);
    expect(sentAfter(0, "POST /api/auth/refresh")).toEqual([]);
    expect(sentAfter(clickAt, "GET /api/auth/me")).toEqual([]);
  }, 20000);

  it("is presented again before a sign-in, which then replaces the session", async () => {
    await onTheIdentityUnavailableScreen();
    meStoreDown = false;
    logoutMode = "500";
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 6000 });

    logoutMode = "ok";
    const before = log.length;
    fireEvent.change(await screen.findByPlaceholderText("Email"), { target: { value: "r@example.test" } });
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 6000 });
    const sent = log.slice(before).filter((l) => / \/api\/auth\/(logout|login)/.test(l));
    expect(sent).toEqual([
      "POST /api/auth/logout [refresh=T0 access=A0]",
      "POST /api/auth/login [refresh=none access=none]",
    ]);
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
  }, 20000);

  it("is presented again before the browser is handed to a single sign-on provider", async () => {
    ssoProviders = [{ name: "github", display: "GitHub" }];
    await onTheIdentityUnavailableScreen();
    meStoreDown = false;
    logoutMode = "500";
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 6000 });

    logoutMode = "ok";
    const before = log.length;
    (await screen.findByRole("button", { name: "Continue with GitHub" })).click();
    await waitFor(() => expect(sentAfter(before, "POST /api/auth/logout")).toEqual(["POST /api/auth/logout [refresh=T0 access=A0]"]));
    expect(rows.get("T0")?.revoked).toBe(true);
  }, 20000);
});
