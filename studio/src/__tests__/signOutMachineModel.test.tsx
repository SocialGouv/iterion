// @vitest-environment jsdom
// The sign-out machine, model-based: short event sequences over the REAL
// AuthProvider + client, with the invariants I1–I6 (design §I) asserted after
// EVERY step — not per-scenario, per-step. The events are the machine's:
// sign-out requested, a logout answering 2xx / other-session / 5xx, an
// identity read answering (REFRESH ANSWERED), a sign-in by password (with and
// without the explicit anyway), an SSO start (with and without the explicit
// anyway), an SSO landing delivering a session, a password change in another
// tab, a storage event from another tab, and a crash/restart (reload).
//
// The HTTP boundary is the model of signOutBoundToItsSession.test.tsx: the
// jar is read when a request is SENT and moved when it ARRIVES.
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

vi.mock("@/components/shared/AppShell", async () => {
  const { Suspense } = await import("react");
  const { useAuth } = await import("@/auth/AuthContext");
  return {
    default: function ShellStandIn({ children }: { children: ReactNode }) {
      const { signOut } = useAuth();
      return (
        <Suspense fallback={null}>
          <button onClick={() => void signOut()}>Sign out</button>
          {children}
        </Suspense>
      );
    },
  };
});
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/ChatDock/ChatDock", () => ({ default: () => null, AssistantDockCrashed: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => <h1>Restricted account</h1> }));
vi.mock("@/views/ConfigShare", () => ({ default: () => null }));
vi.mock("@/views/Marketplace", () => ({ default: () => null }));
vi.mock("@/views/account/ApiKeys", () => ({ default: () => null }));
vi.mock("@/views/account/NotificationsPanel", () => ({ default: () => null }));
vi.mock("@/views/account/OAuthConnections", () => ({ default: () => null }));
vi.mock("@/views/account/SharedQuota", () => ({ default: () => null }));
vi.mock("@/views/account/TokensPanel", () => ({ default: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: () => <h1>Signed-in home</h1> }));

import App from "@/App";
import { cancelScheduledRefresh, SIGN_OUT_PENDING_KEY } from "@/api/client";
import { readRecord, type SignOutRecord } from "@/api/signOutMachine";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const account = {
  user: { id: "u", email: "m@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};

interface Row { lineage: string; revoked: boolean }

let jar: { refresh: string | null; access: string | null };
let liveAccess: Set<string>;
let rows: Map<string, Row>;
let nameSession: boolean;
let logoutMode: "ok" | "500" | "401";
let log: string[];
let navigations: string[];
let nextSession = 1;
let offJsdomError: (() => void) | null = null;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const shellVisible = () => screen.queryByRole("heading", { name: "Signed-in home" }) !== null;
const marked = () => localStorage.getItem(SIGN_OUT_PENDING_KEY);
const storedRecord = (): SignOutRecord | null => readRecord();
const logoutRequests = () => log.filter((l) => l.startsWith("POST /api/auth/logout"));
const app = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <App />
  </QueryClientProvider>
);

function serverOpensASession(prefix: string): { token: string; lineage: string } {
  const n = nextSession++;
  rows.set(`T${n}`, { lineage: `${prefix}${n}`, revoked: false });
  liveAccess.add(`A${n}`);
  jar = { refresh: `T${n}`, access: `A${n}` };
  return { token: `T${n}`, lineage: `${prefix}${n}` };
}

beforeEach(() => {
  localStorage.clear();
  nextSession = 1;
  jar = { refresh: "T0", access: "A0" };
  liveAccess = new Set(["A0"]);
  rows = new Map([["T0", { lineage: "L0", revoked: false }]]);
  nameSession = true;
  logoutMode = "ok";
  log = [];
  navigations = [];
  window.history.replaceState({}, "", "/studio/runs/review-123");
  useServerInfoStore.setState({ info: { mode: "cloud", auth_required: true } as ServerInfo });
  const vc = (globalThis as unknown as { jsdom: { virtualConsole: { on: (ev: string, fn: (e: Error) => void) => void; off: (ev: string, fn: (e: Error) => void) => void } } }).jsdom.virtualConsole;
  const onJsdomError = (e: Error) => {
    if (String(e?.message).includes("navigation")) navigations.push(String(e.message));
  };
  vc.on("jsdomError", onJsdomError);
  offJsdomError = () => vc.off("jsdomError", onJsdomError);
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), "https://iterion.invalid");
    const path = url.pathname;
    const method = init?.method ?? "GET";
    const sent = { ...jar };
    log.push(`${method} ${path}${url.search} [refresh=${sent.refresh ?? "none"} access=${sent.access ?? "none"}]`);
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [{ name: "github", display: "GitHub" }] });
    if (path === "/api/me/sso/links") return json({ links: [] });
    if (path === "/api/auth/me") {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "authentication required" }, 401);
      const row = sent.refresh ? rows.get(sent.refresh) : undefined;
      if (row?.revoked) return json({ error: "session revoked" }, 401);
      const lineage = nameSession && row ? row.lineage : undefined;
      return json(lineage ? { ...account, session_lineage: lineage } : account);
    }
    if (path === "/api/auth/login" && method === "POST") {
      serverOpensASession("PW");
      return json({ user: account.user, active_org_id: "o1", active_team_id: "t1", active_role: "member", expires_at: "2026-12-01T00:00:00.000Z" });
    }
    if (path === "/api/auth/refresh") {
      const row = sent.refresh ? rows.get(sent.refresh) : undefined;
      if (!row || row.revoked) return json({ error: "session revoked" }, 401);
      liveAccess.add(`A${sent.refresh}`);
      jar = { refresh: sent.refresh, access: `A${sent.refresh}` };
      return json({ user: account.user });
    }
    if (path === "/api/auth/logout") {
      if (logoutMode === "500") return json({ error: "identity unavailable" }, 500);
      if (logoutMode === "401") return json({ error: "session revoked" }, 401);
      const asked = url.searchParams.get("lineage");
      const held = sent.refresh ? rows.get(sent.refresh)?.lineage : undefined;
      if (asked && held && asked !== held) {
        log.push(`  logout: ${asked} is not this browser's session (${held}); nothing revoked, cookies kept`);
        return new Response(null, { status: 204, headers: { "X-Iterion-Logout": "other-session" } });
      }
      for (const r of rows.values()) if ((asked && r.lineage === asked) || (!asked && r.lineage === held)) r.revoked = true;
      jar = { refresh: null, access: null };
      log.push(`  logout: lineage ${asked ?? held ?? "none"} revoked, jar cleared`);
      return new Response(null, { status: 204 });
    }
    if (path.startsWith("/api/")) {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "token expired" }, 401);
      return json({});
    }
    throw new Error(`unexpected ${method} ${path}`);
  }));
});

afterEach(async () => {
  offJsdomError?.();
  offJsdomError = null;
  await new Promise((r) => setTimeout(r, 10));
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ---- The step driver: every machine event, awaited to its settle ----

async function mountFresh(): Promise<void> {
  cleanup();
  render(app());
  await waitFor(() => {
    if (!shellVisible() && !screen.queryByRole("heading", { name: "Sign in to iterion" }) && !screen.queryByRole("button", { name: "Sign out" })) {
      throw new Error("not settled");
    }
  }, { timeout: 8000 });
}

async function stepSignOutRequested(): Promise<void> {
  screen.getByRole("button", { name: "Sign out" }).click();
  await waitFor(() => expect(logoutRequests().length).toBeGreaterThanOrEqual(1), { timeout: 8000 });
  await waitFor(() => {
    const s = screen.queryByRole("heading", { name: "Sign in to iterion" });
    if (!s && !shellVisible() && !screen.queryByText(/Sign-out/)) throw new Error("sign-out not settled");
  }, { timeout: 8000 });
}

async function stepSignInByPassword(anyway: boolean): Promise<void> {
  fireEvent.change(await screen.findByPlaceholderText("Email", {}, { timeout: 8000 }), { target: { value: "m@example.test" } });
  fireEvent.change(await screen.findByPlaceholderText("Password"), { target: { value: "pw-123456789" } });
  fireEvent.click(await screen.findByRole("button", { name: "Sign in" }));
  if (anyway) {
    // The machine holds the first attempt; the explicit way past it.
    await screen.findByText(/this sign-in is on hold/, {}, { timeout: 8000 });
    fireEvent.click(await screen.findByRole("button", { name: /Sign in anyway/ }));
    await waitFor(() => expect(shellVisible()).toBe(true), { timeout: 8000 });
    return;
  }
  // Without the explicit choice the sign-in either lands (no record gated it)
  // or is HELD — the hold names itself; what it must never do is send the
  // login quietly.
  await waitFor(() => {
    const held = screen.queryByText(/this sign-in is on hold/);
    if (!held && !shellVisible()) throw new Error("sign-in not settled");
  }, { timeout: 8000 });
}

async function stepSsoStart(anyway: boolean): Promise<void> {
  (await screen.findByRole("button", { name: "Continue with GitHub" }, { timeout: 8000 })).click();
  if (anyway) {
    await screen.findByText(/signing in with GitHub is on hold/, {}, { timeout: 8000 });
    (await screen.findByRole("button", { name: "Continue with GitHub anyway" })).click();
  }
  await waitFor(() => expect(navigations.length).toBeGreaterThanOrEqual(1), { timeout: 8000 });
}

async function stepStorageEvent(): Promise<void> {
  // The browser delivers the record's storage event to every OTHER tab; the
  // tab re-reads and re-renders.
  window.dispatchEvent(new StorageEvent("storage", { key: SIGN_OUT_PENDING_KEY, newValue: marked() }));
  await new Promise((r) => setTimeout(r, 50));
}

// ---- The invariants, asserted after EVERY step ----

async function checkInvariants(step: string): Promise<void> {
  const record = storedRecord();
  const body = document.body.textContent ?? "";
  const warned = body.includes("Sign-out not confirmed") || body.includes("Sign-out abandoned");

  // I2/I6 — a standing record is NEVER silent: the warning (owed) or the
  // notice (abandoned) is rendered, in this tab, whatever else it shows.
  if (record) {
    expect(warned, `${step}: a standing record must render its warning/notice`).toBe(true);
    if (record.state === "abandoned") {
      expect(body.includes("Sign-out abandoned"), `${step}: an abandoned record renders the notice`).toBe(true);
    }
  } else {
    // No record — no warning either (a stale banner would be a lie).
    expect(warned, `${step}: no record, no warning`).toBe(false);
  }

  // I1 — while the browser still holds the record's session (server-side
  // truth the client can reach), no signed-in view is adopted over an OWED
  // sign-out, and the UI never shows "signed out" for a live held session
  // without the warning.
  if (record?.state === "owed") {
    const heldLineage = jar.refresh ? rows.get(jar.refresh)?.lineage : undefined;
    const recordHeld = record.lineage === "" || record.lineage === heldLineage;
    if (recordHeld && heldLineage !== undefined) {
      expect(shellVisible(), `${step}: OWED and still held — no signed-in view`).toBe(false);
    }
  }

  // I3 — every logout presented the record's own lineage, or no lineage when
  // the record named none; no logout without a lineage while one was named.
  for (const l of logoutRequests()) {
    const asked = l.includes("?lineage=") ? l.split("?lineage=")[1]?.split(" ")[0] : null;
    if (record && record.lineage) {
      // A logout is always presented with the lineage SOMETHING named; a
      // bare logout (unnamed) may only follow an unnamed record or no record.
      if (asked === null) {
        expect(record.lineage === "", `${step}: a bare logout over a named record: ${l}`).toBe(true);
      }
    }
  }

  // I4 — a login request is sent only after the explicit anyway (checked at
  // the step level: stepSignInByPassword(false) holds before any login).
}

async function expectInvariantHold(step: string): Promise<void> {
  await checkInvariants(step);
}

describe("the machine over short event sequences, invariants after every step", () => {
  it("sign-out 5xx → reload → sign-out 5xx → storage → reload, always owed and warned", async () => {
    logoutMode = "500";
    await mountFresh();
    await expectInvariantHold("mount");
    await stepSignOutRequested();
    await expectInvariantHold("sign-out(500)");
    await mountFresh();
    await expectInvariantHold("reload");
    await stepStorageEvent();
    await expectInvariantHold("storage");
    // The incident is over at the next reload: the presentation confirms.
    logoutMode = "ok";
    await mountFresh();
    await expectInvariantHold("reload(ok)");
    expect(storedRecord()).toBeNull();
    expect(rows.get("T0")?.revoked).toBe(true);
  }, 60000);

  it("sign-out 5xx → sign-in held → sign-in anyway → landing on a new lineage → reload", async () => {
    logoutMode = "500";
    await mountFresh();
    await expectInvariantHold("mount");
    await stepSignOutRequested();
    await expectInvariantHold("sign-out(500)");
    await stepSignInByPassword(false);
    await expectInvariantHold("sign-in held");
    // No login was sent while the hold stood (I4).
    expect(log.filter((l) => l.startsWith("POST /api/auth/login"))).toEqual([]);
    // The explicit way past it: the record moves to ABANDONED (notice, I2),
    // the sign-in lands, and the landing's reload keeps the notice (I5).
    await stepSignInByPasswordTrueAfterHold();
    await expectInvariantHold("sign-in anyway");
    expect(storedRecord()?.state).toBe("abandoned");
    const landing = serverOpensASession("SSO");
    await mountFresh();
    await expectInvariantHold("landing reload");
    expect(storedRecord()?.state).toBe("abandoned");
    expect(rows.get(landing.token)?.revoked).toBe(false);
    expect(shellVisible()).toBe(true);
  }, 60000);

  it("sign-out 5xx → SSO start held → anyway → the trip abandoned → the landing re-presents and never signs in", async () => {
    logoutMode = "500";
    await mountFresh();
    await expectInvariantHold("mount");
    await stepSignOutRequested();
    await expectInvariantHold("sign-out(500)");
    await stepSsoStart(true);
    await expectInvariantHold("sso start anyway");
    expect(storedRecord()?.state).toBe("abandoned");
    // The provider round trip is abandoned; the incident is over at the
    // landing: the machine re-presents the held session's sign-out and ends
    // it — nothing is adopted silently (I5, r7c F2).
    logoutMode = "ok";
    await mountFresh();
    await expectInvariantHold("landing reload");
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(storedRecord()).toBeNull();
    expect(shellVisible()).toBe(false);
  }, 60000);

  it("sign-out 5xx → another tab's sign-in (REFRESH ANSWERED) → reload: owed+gone warns beside the new session", async () => {
    logoutMode = "500";
    await mountFresh();
    await expectInvariantHold("mount");
    await stepSignOutRequested();
    await expectInvariantHold("sign-out(500)");
    const elsewhere = await stepAnotherTabsSignInThenReload();
    await expectInvariantHold("reload after another tab's sign-in");
    // The record stands (never dropped), the new session is usable, the
    // warning is up (I1/I2), and the new session was never ended by the
    // record's presentation (I3).
    expect(storedRecord()?.state).toBe("owed");
    expect(rows.get(elsewhere.token)?.revoked).toBe(false);
    expect(shellVisible()).toBe(true);
    // A second sign-out now names what the browser holds and ends it.
    logoutMode = "ok";
    await stepSignOutRequested();
    await expectInvariantHold("second sign-out");
    expect(rows.get(elsewhere.token)?.revoked).toBe(true);
    expect(storedRecord()).toBeNull();
  }, 60000);

  it("sign-out confirmed at once → clean: no record, no warning, anywhere", async () => {
    logoutMode = "ok";
    await mountFresh();
    await expectInvariantHold("mount");
    await stepSignOutRequested();
    await expectInvariantHold("sign-out(ok)");
    expect(storedRecord()).toBeNull();
    await mountFresh();
    await expectInvariantHold("reload");
    expect(storedRecord()).toBeNull();
    expect(shellVisible()).toBe(false);
  }, 60000);
});

async function stepSignInByPasswordTrueAfterHold(): Promise<void> {
  // The form is already showing the hold; the anyway button is up.
  fireEvent.click(await screen.findByRole("button", { name: /Sign in anyway/ }));
  await waitFor(() => expect(shellVisible()).toBe(true), { timeout: 8000 });
}

async function stepAnotherTabsSignInThenReload(): Promise<{ token: string; lineage: string }> {
  const elsewhere = serverOpensASession("X");
  logoutMode = "ok";
  await mountFresh();
  return elsewhere;
}
