// @vitest-environment jsdom
// The sign-out state machine (studio half) — the transition witnesses. One
// record per browser, written BEFORE the logout it guards, read by every tab:
// OWED warns while the session may live (I1), an abandonment keeps the record
// with a visible notice (I2), a sign-in over an unconfirmed record is held
// behind the explicit "anyway" (I4), a reload finds the record and resolves
// it before any identity is adopted (I5), and every tab re-renders from the
// record on a storage event (I6).
//
// The HTTP boundary is modelled as in signOutBoundToItsSession.test.tsx: the
// jar is read when a request is SENT and moved when it ARRIVES, and the
// logout implements the server contract — 204 plain ends the named lineage
// and clears the cookies, 204 + X-Iterion-Logout: other-session revokes
// nothing and keeps them, 5xx/401/transport decide nothing.
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useRoute } from "wouter";

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
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: function RequestedRun() {
  const [, params] = useRoute("/runs/:id");
  return <h1>Requested run {params?.id}</h1>;
} }));

import App from "@/App";
import { cancelScheduledRefresh, SIGN_OUT_PENDING_KEY } from "@/api/client";
import { readRecord, heldnessOf, SESSION_RENEWED_AT_KEY, type SignOutRecord } from "@/api/signOutMachine";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const account = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};

interface Row { lineage: string; revoked: boolean }

const SESSION_EXPIRES_AT = "2026-12-01T00:00:00.000Z";

let jar: { refresh: string | null; access: string | null };
let liveAccess: Set<string>;
let rows: Map<string, Row>;
let nameSession: boolean;
// The session bound the /auth/me answer names, once the server sends one:
// undefined = the field is absent (the answer names no bound).
let meExpiresAt: string | undefined;
let logoutMode: "ok" | "500" | "401" | "held" | "fail-unnamed";
let heldLogoutResolvers: Array<(res: Response) => void>;
let cleanupFails: boolean;
let log: string[];
let navigations: string[];
let nextSession = 1;
let offJsdomError: (() => void) | null = null;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const shell = () => screen.queryByRole("heading", { name: /Requested run|Signed-in home/ });
const marked = () => localStorage.getItem(SIGN_OUT_PENDING_KEY);
const storedRecord = (): SignOutRecord | null => {
  const raw = marked();
  if (!raw) {
    const inMemory = readRecord();
    return inMemory;
  }
  return JSON.parse(raw) as SignOutRecord;
};
const logoutsSent = () => log.filter((l) => l.startsWith("POST /api/auth/logout"));
const loginsSent = () => log.filter((l) => l.startsWith("POST /api/auth/login"));
const app = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <App />
  </QueryClientProvider>
);

// serverOpensASession is a session the server opens for this browser and whose
// cookies it sets: a credential route's answer, or one delivered to another
// tab of the same browser — the jar is shared.
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
  meExpiresAt = undefined;
  logoutMode = "ok";
  heldLogoutResolvers = [];
  cleanupFails = false;
  log = [];
  navigations = [];
  window.history.replaceState({}, "", "/studio/runs/review-123");
  useServerInfoStore.setState({ info: { mode: "cloud", auth_required: true } as ServerInfo });
  // jsdom reports an attempted top-level navigation (window.location.href =
  // ...) through its virtual console; record it as the redirect.
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
      return json({
        ...account,
        ...(lineage ? { session_lineage: lineage } : {}),
        ...(meExpiresAt !== undefined ? { expires_at: meExpiresAt } : {}),
      });
    }
    if (path === "/api/auth/login" && method === "POST") {
      const s = serverOpensASession("PW");
      log.push(`  login: new session ${s.token} (${s.lineage}) set, expires ${SESSION_EXPIRES_AT}`);
      return json({ user: account.user, active_org_id: "o1", active_team_id: "t1", active_role: "member", expires_at: SESSION_EXPIRES_AT });
    }
    if (path === "/api/auth/refresh") {
      const row = sent.refresh ? rows.get(sent.refresh) : undefined;
      if (!row || row.revoked) return json({ error: "session revoked" }, 401);
      liveAccess.add(`A${sent.refresh}`);
      jar = { refresh: sent.refresh, access: `A${sent.refresh}` };
      return json({ user: account.user, expires_at: SESSION_EXPIRES_AT });
    }
    if (path === "/api/auth/logout") {
      if (logoutMode === "held") return new Promise<Response>((resolve) => { heldLogoutResolvers.push(resolve); });
      if (logoutMode === "500") return json({ error: "identity unavailable" }, 500);
      if (logoutMode === "401") return json({ error: "session revoked" }, 401);
      const asked = url.searchParams.get("lineage");
      const held = sent.refresh ? rows.get(sent.refresh)?.lineage : undefined;
      if (logoutMode === "fail-unnamed" && !asked) return json({ error: "identity unavailable" }, 500);
      if (asked && held && asked !== held) {
        log.push(`  logout: ${asked} is not this browser's session (${held}); nothing revoked, cookies kept`);
        return new Response(null, { status: 204, headers: { "X-Iterion-Logout": "other-session" } });
      }
      for (const r of rows.values()) if ((asked && r.lineage === asked) || (!asked && r.lineage === held)) r.revoked = true;
      jar = { refresh: null, access: null };
      log.push(`  logout: lineage ${asked ?? held ?? "none"} revoked, jar cleared`);
      return new Response(null, { status: 204 });
    }
    if (path === "/api/me/sessions/revoke-all" && method === "POST") {
      if (cleanupFails) {
        // A cleanup step failing AFTER the sign-out landed.
        return json({ error: "sign-out done; cleanup incomplete — sign in again and repeat sign out everywhere", error_code: "signout_cleanup_failed" }, 503);
      }
      for (const r of rows.values()) r.revoked = true;
      liveAccess.clear();
      jar = { refresh: null, access: null };
      log.push("  revoke-all: every session revoked, this browser's cookies cleared");
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
  // A logout left on the wire is answered, so no presentation of this test
  // survives it blocking the next one's renewals.
  for (const resolve of heldLogoutResolvers.splice(0)) resolve(new Response(null, { status: 204 }));
  await new Promise((r) => setTimeout(r, 10));
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function signOutUnconfirmed(): Promise<void> {
  render(app());
  await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
  screen.getByRole("button", { name: "Sign out" }).click();
  await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
}

// A sign-in through the form: the credential answer carries the session's
// expiry, so the machine's record can name it — the identity a bootstrap read
// adopted names only the lineage.
async function signedInThroughTheForm(): Promise<{ token: string; lineage: string }> {
  jar = { refresh: null, access: null };
  render(app());
  await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
  fireEvent.change(await screen.findByPlaceholderText("Email"), { target: { value: "r@example.test" } });
  fireEvent.change(await screen.findByPlaceholderText("Password"), { target: { value: "pw-123456789" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
  return { token: String(jar.refresh), lineage: rows.get(String(jar.refresh))!.lineage };
}

async function signOutUnconfirmedAfterFormSignIn(): Promise<void> {
  await signedInThroughTheForm();
  screen.getByRole("button", { name: "Sign out" }).click();
  await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
}

// What a reload / a new tab / the next person at the computer comes to.
async function aReloadComesTo(): Promise<"signed-in" | "signed-out"> {
  cleanup();
  render(app());
  await waitFor(() => {
    if (!shell() && !screen.queryByRole("heading", { name: "Sign in to iterion" }) && !screen.queryByRole("button", { name: "Sign out" })) {
      throw new Error("not settled");
    }
  }, { timeout: 8000 });
  return shell() ? "signed-in" : "signed-out";
}

describe("the record and its write ordering", () => {
  // I5: every live state is persisted before the action it guards.
  it("is written before the logout goes on the wire, and a lost answer leaves it standing", async () => {
    logoutMode = "held"; // the logout never answers
    const delivered = await signedInThroughTheForm();
    screen.getByRole("button", { name: "Sign out" }).click();
    // The record is durable before the request it guards: a tab closed now,
    // or a logout that never answers, still leaves it.
    await waitFor(() => expect(storedRecord()).not.toBeNull(), { timeout: 8000 });
    const written = storedRecord()!;
    expect(logoutsSent()).toHaveLength(1);
    expect(written.state).toBe("owed");
    expect(written.lineage).toBe(delivered.lineage);
    expect(logoutsSent()[0]).toContain(`?lineage=${delivered.lineage}`);
    expect(written.account).toBe("u");
    expect(written.expires_at).toBe(SESSION_EXPIRES_AT);
    // The window closes at the bound: signed out locally, still owed.
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 9000 });
    expect(storedRecord()?.state).toBe("owed");
    expect(rows.get(delivered.token)?.revoked).toBe(false);
  }, 30000);

  it("carries the session expiry, which the notices name", async () => {
    logoutMode = "500";
    await signedInThroughTheForm();
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    expect(storedRecord()?.expires_at).toBe(SESSION_EXPIRES_AT);
    expect(document.body.textContent).toContain(SESSION_EXPIRES_AT);
  }, 30000);

  // The identity read names the session's bound too — a sign-out asked after
  // a reload (where no session answer ran) carries a real date, and the
  // machine's expiry rule has a bound to act on.
  it("takes the expiry the /auth/me answer names, and drops the record past that bound", async () => {
    logoutMode = "500";
    meExpiresAt = "2020-01-01T00:00:00.000Z";
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    expect(storedRecord()?.expires_at).toBe("2020-01-01T00:00:00.000Z");
    // The reload: the record is past its own bound — dropped, nothing live
    // is being hidden; the session the mock keeps alive is read like any
    // other.
    const next = await aReloadComesTo();
    expect(next).toBe("signed-in");
    expect(storedRecord()).toBeNull();
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
  }, 30000);
});

describe("another tab's event (I6 — the storage re-render, the real witness the phantom listener never was)", () => {
  it("warns this tab while the session lives, from the record alone", async () => {
    // This tab is signed in; another tab's sign-out writes the OWED record.
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    const otherTab: SignOutRecord = {
      state: "owed", lineage: "L0", account: "u", expires_at: SESSION_EXPIRES_AT,
      since: Date.now(), abandoned_at: null, resolved_gone_at: null,
    };
    localStorage.setItem(SIGN_OUT_PENDING_KEY, JSON.stringify(otherTab));
    window.dispatchEvent(new StorageEvent("storage", { key: SIGN_OUT_PENDING_KEY, newValue: marked() }));
    // The warning renders HERE, while this tab still shows the account — and
    // this tab signs out too (the jar is shared): its own logout reaches the
    // server even when the other tab's cannot.
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    await waitFor(() => expect(logoutsSent()).toHaveLength(1), { timeout: 8000 });
    expect(logoutsSent()[0]).toContain("?lineage=L0");
    // The confirmation here clears the shared record: the warning lifts.
    await waitFor(() => expect(storedRecord()).toBeNull(), { timeout: 8000 });
  }, 30000);

  it("shows the abandoned notice in this tab when another tab goes on without the sign-out (r7c F2, I2)", async () => {
    logoutMode = "500";
    await signOutUnconfirmedAfterFormSignIn();
    const bannerBefore = screen.getByText("Sign-out not confirmed");
    expect(bannerBefore).toBeTruthy();
    // Another tab's "continue anyway" moves the record to ABANDONED — it does
    // NOT remove it, so this tab's warning is not silenced: it becomes the
    // notice.
    const before = JSON.parse(marked()!) as SignOutRecord;
    const abandoned: SignOutRecord = { ...before, state: "abandoned", abandoned_at: Date.now() };
    localStorage.setItem(SIGN_OUT_PENDING_KEY, JSON.stringify(abandoned));
    window.dispatchEvent(new StorageEvent("storage", { key: SIGN_OUT_PENDING_KEY, newValue: marked() }));
    await screen.findByText("Sign-out abandoned — that session is still active", {}, { timeout: 8000 });
    expect(screen.getByText("Sign-out abandoned — that session is still active")).toBeTruthy();
    expect(document.body.textContent).toContain(SESSION_EXPIRES_AT);
    // And the browser still holds the signed-in session: a reload refuses to
    // adopt it as a plain sign-in (the next describe covers the landing).
    expect(jar.refresh).toBeTruthy();
  }, 30000);

  // A tab frozen through a record write (suspended background tab) processes
  // no storage event; becoming visible again re-reads the record and
  // re-renders from it (I6) — the event alone edits nothing.
  it("re-reads the record when the tab becomes visible again", async () => {
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    // Another tab's sign-out wrote the OWED record while this tab was
    // frozen: no storage event is dispatched here.
    const otherTab: SignOutRecord = {
      state: "owed", lineage: "L0", account: "u", expires_at: SESSION_EXPIRES_AT,
      since: Date.now(), abandoned_at: null, resolved_gone_at: null,
    };
    localStorage.setItem(SIGN_OUT_PENDING_KEY, JSON.stringify(otherTab));
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
    document.dispatchEvent(new Event("visibilitychange"));
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    // The re-read never clears or edits the record.
    expect(storedRecord()?.state).toBe("owed");
    expect(rows.get("T0")?.revoked).toBe(false);
  }, 30000);

  it("re-reads the record when the window is focused again", async () => {
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    const otherTab: SignOutRecord = {
      state: "owed", lineage: "L0", account: "u", expires_at: SESSION_EXPIRES_AT,
      since: Date.now(), abandoned_at: null, resolved_gone_at: null,
    };
    localStorage.setItem(SIGN_OUT_PENDING_KEY, JSON.stringify(otherTab));
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
    window.dispatchEvent(new FocusEvent("focus"));
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    expect(storedRecord()?.state).toBe("owed");
  }, 30000);
});

describe("SIGN-IN BY PASSWORD over an unconfirmed sign-out (r7c F3, I4)", () => {
  it("is held, sends no login, and lands only on the explicit sign-in-anyway", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    fireEvent.change(await screen.findByPlaceholderText("Email"), { target: { value: "r@example.test" } });
    fireEvent.change(await screen.findByPlaceholderText("Password"), { target: { value: "pw-123456789" } });
    fireEvent.click(await screen.findByRole("button", { name: "Sign in" }));
    // The machine presents the sign-out first (a second logout goes out) and
    // holds the sign-in: no login request is sent, the form says what holds
    // it and names the expiry.
    await screen.findByText(/this sign-in is on hold/, {}, { timeout: 8000 });
    await waitFor(() => expect(logoutsSent()).toHaveLength(2), { timeout: 8000 });
    expect(loginsSent()).toEqual([]);
    expect(screen.queryByRole("heading", { name: "Requested run review-123" })).toBeNull();
    const heldText = document.body.textContent ?? "";
    expect(heldText).toContain(SESSION_EXPIRES_AT);

    // The explicit way past it: the record moves to ABANDONED (it STANDS,
    // with its notice, I2) and the sign-in lands.
    fireEvent.click(screen.getByRole("button", { name: /Sign in anyway/ }));
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    expect(loginsSent()).toHaveLength(1);
    const record = storedRecord()!;
    expect(record.state).toBe("abandoned");
    expect(record.lineage).toBe("L0");
    expect(rows.get("T0")?.revoked).toBe(false);
    // The notice persists beside the new session (I4's studio half: the
    // record is never silently dropped by the sign-in it gated).
    expect(screen.getByText("Sign-out abandoned — that session is still active")).toBeTruthy();
  }, 40000);

  it("proceeds with the notice when the record's session is no longer held (+GONE)", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    // Another tab's sign-in replaces the jar; the record resolves GONE at the
    // next read — which is the reload's presentation, answering other-session
    // now that the incident is over. The sign-in then proceeds: the record
    // only warns.
    serverOpensASession("X");
    logoutMode = "ok";
    cleanup();
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    // The bootstrap's presentation answered other-session for L0 and the held
    // session was adopted under the standing warning.
    expect(shell()).not.toBeNull();
    const record = storedRecord()!;
    expect(record.state).toBe("owed");
    expect(heldnessOf(record)).toBe("gone");
    expect(screen.getByText("Sign-out not confirmed")).toBeTruthy();
  }, 30000);
});

describe("SSO START and the OIDC LANDING (r7c F2, I1/I2/I5)", () => {
  it("holds the start while the named session is held, and 'Continue anyway' abandons without silencing the other tabs", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    const before = logoutsSent().length;
    (await screen.findByRole("button", { name: "Continue with GitHub" })).click();
    // HELD: the presentation went out once more and failed; no redirect.
    await screen.findByText(/signing in with GitHub is on hold/, {}, { timeout: 8000 });
    await waitFor(() => expect(logoutsSent()).toHaveLength(before + 1), { timeout: 8000 });
    expect(navigations).toEqual([]);

    // The way on: ABANDONED (the record stands), and the redirect goes.
    screen.getByRole("button", { name: "Continue with GitHub anyway" }).click();
    await waitFor(() => expect(navigations.length).toBe(1), { timeout: 8000 });
    // The abandonment KEEPS the record (I2): it is never a removal.
    expect(marked()).not.toBeNull();
    const record = storedRecord()!;
    expect(record.state).toBe("abandoned");
    expect(record.lineage).toBe("L0");
    expect(rows.get("T0")?.revoked).toBe(false);
  }, 40000);

  it("an abandoned provider trip that lands back on the held lineage re-presents and never signs in", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    (await screen.findByRole("button", { name: "Continue with GitHub" })).click();
    await screen.findByText(/signing in with GitHub is on hold/, {}, { timeout: 8000 });
    screen.getByRole("button", { name: "Continue with GitHub anyway" }).click();
    await waitFor(() => expect(navigations.length).toBe(1), { timeout: 8000 });

    // The user comes back without finishing at the provider (Back, a closed
    // tab, a provider error) — and the incident is over: the landing's
    // re-presentation confirms the sign-out, so nothing is adopted.
    logoutMode = "ok";
    const next = await aReloadComesTo();
    expect(next).toBe("signed-out");
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(storedRecord()).toBeNull();
  }, 40000);

  it("an abandoned trip landing on an unconfirmable sign-out stays out and keeps the notice", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    (await screen.findByRole("button", { name: "Continue with GitHub" })).click();
    await screen.findByText(/signing in with GitHub is on hold/, {}, { timeout: 8000 });
    screen.getByRole("button", { name: "Continue with GitHub anyway" }).click();
    await waitFor(() => expect(navigations.length).toBe(1), { timeout: 8000 });

    // Still no confirmation at the landing: the browser stays on the
    // signed-out page — the held session is NOT adopted as a plain sign-in —
    // and the abandonment notice stands.
    const next = await aReloadComesTo();
    expect(next).toBe("signed-out");
    await screen.findByText("Sign-out abandoned — that session is still active", {}, { timeout: 8000 });
    expect(rows.get("T0")?.revoked).toBe(false);
    expect(jar.refresh).toBe("T0");
    const record = storedRecord()!;
    expect(record.state).toBe("abandoned");
  }, 40000);

  it("a landing that delivers a NEW lineage proceeds and leaves the record ABANDONED+GONE with its notice", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    (await screen.findByRole("button", { name: "Continue with GitHub" })).click();
    await screen.findByText(/signing in with GitHub is on hold/, {}, { timeout: 8000 });
    screen.getByRole("button", { name: "Continue with GitHub anyway" }).click();
    await waitFor(() => expect(navigations.length).toBe(1), { timeout: 8000 });

    // The provider's callback sets its cookies; the incident is over.
    logoutMode = "ok";
    const delivered = serverOpensASession("SSO");
    const next = await aReloadComesTo();
    expect(next).toBe("signed-in");
    // The landing presented L0 (nothing revoked: it is not this browser's
    // session any more), adopted the delivered one, and the record stands
    // ABANDONED+GONE — the notice survives (I2), the sign-in was never
    // silently taken (I4 held it until the explicit choice).
    const record = storedRecord()!;
    expect(record.state).toBe("abandoned");
    expect(heldnessOf(record)).toBe("gone");
    expect(rows.get("T0")?.revoked).toBe(false);
    expect(rows.get(delivered.token)?.revoked).toBe(false);
    expect(screen.getByText("Sign-out abandoned — that session is still active")).toBeTruthy();
  }, 40000);
});

describe("the presentation loop (r6c F3, I1)", () => {
  it("keeps holding and never fake-confirms: a 5xx answer leaves the record, a dead credential ends it", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    expect(storedRecord()?.state).toBe("owed");
    // The loop's retries never turn an error into a confirmation.
    await new Promise((r) => setTimeout(r, 2400));
    expect(storedRecord()?.state).toBe("owed");
    expect(rows.get("T0")?.revoked).toBe(false);

    // A refusal that names the presented credential itself dead IS the end
    // (I1's dead-credential exception): the record goes, the warning lifts.
    logoutMode = "401";
    await new Promise((r) => setTimeout(r, 5000));
    await waitFor(() => expect(storedRecord()).toBeNull(), { timeout: 9000 });
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
  }, 40000);
});

describe("CRASH/RESTART (r6c F4/F7, I5)", () => {
  it("a reload finds the OWED record, re-presents it before any signed-in view, and stays out while it is unconfirmed", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    logoutMode = "500";
    const next = await aReloadComesTo();
    expect(next).toBe("signed-out");
    // The reload re-presented (the second presentation is the reload's).
    expect(logoutsSent().length).toBeGreaterThanOrEqual(2);
    expect(storedRecord()?.state).toBe("owed");
    expect(rows.get("T0")?.revoked).toBe(false);
    expect(screen.getByText("Sign-out not confirmed")).toBeTruthy();
  }, 40000);

  it("a reload confirms an OWED record whose presentation now answers, then reads the held session", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    logoutMode = "ok";
    const next = await aReloadComesTo();
    expect(next).toBe("signed-out");
    // Confirmed: the record cleared, the cookies went with it, the signed-out
    // page is the honest state — no signed-in view is adopted over it.
    expect(storedRecord()).toBeNull();
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(jar).toEqual({ refresh: null, access: null });
  }, 40000);

  it("a record past its own expiry is dropped: nothing live is being hidden", async () => {
    logoutMode = "500";
    await signOutUnconfirmed();
    const record = JSON.parse(marked()!) as SignOutRecord;
    record.expires_at = "2020-01-01T00:00:00.000Z";
    localStorage.setItem(SIGN_OUT_PENDING_KEY, JSON.stringify(record));
    const next = await aReloadComesTo();
    // The record is dropped at the bootstrap: past its own bound, it hides
    // nothing live (I1's exception, documented). The jar's session — which the
    // mock keeps alive past that bound — is then read like any session's.
    expect(next).toBe("signed-in");
    expect(storedRecord()).toBeNull();
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
  }, 40000);
});

describe("an UNNAMED record whose cookies were replaced since the ask", () => {
  it("cannot present at the reload: the delivery proceeds under the notice", async () => {
    // The credential named no session: the sign-out is UNNAMED, and an
    // unnamed logout ends whatever the jar holds.
    nameSession = false;
    logoutMode = "500";
    await signOutUnconfirmed();
    expect(storedRecord()?.lineage).toBe("");
    // Another tab signs in (or an SSO landing): the jar moves, and the
    // renewal is stamped for the other tabs. An unnamed sign-out may only be
    // presented with the credential it was asked FROM — which is gone.
    serverOpensASession("X");
    localStorage.setItem(SESSION_RENEWED_AT_KEY, String(Date.now()));
    // The reload: the machine reads the stamp, resolves the record GONE —
    // without presenting it — and adopts the delivered session under the
    // notice. The delivered session is never ended by the stale ask (I3).
    logoutMode = "ok";
    const bareBefore = logoutsSent().length;
    const next = await aReloadComesTo();
    expect(next).toBe("signed-in");
    expect(logoutsSent().length).toBe(bareBefore);
    expect(rows.get(String(jar.refresh))?.revoked).toBe(false);
    const record = storedRecord()!;
    expect(record.state).toBe("abandoned");
    expect(screen.getByText("Sign-out abandoned — that session is still active")).toBeTruthy();
  }, 40000);
});

describe("a sign-out whose lineage moved (I3)", () => {
  it("ends only the session it names, and warns that the browser holds another one", async () => {
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    // Another tab signs in (or an SSO landing): the shared jar moves to T1
    // while THIS tab's identity read still names L0.
    const elsewhere = serverOpensASession("X");
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    // ONE presentation, naming L0: nothing else is ended.
    expect(logoutsSent()).toHaveLength(1);
    expect(logoutsSent()[0]).toContain("?lineage=L0");
    expect(rows.get(elsewhere.token)?.revoked).toBe(false);
    // The record stands resolved GONE — the other-session answer is its
    // resolution — and the tab is on the signed-out page: the ask was NOT
    // confirmed, and nothing ends the session that replaced the named one
    // (I3). The way on is a NEW ask, naming what the next read holds.
    const record = storedRecord()!;
    expect(record.lineage).toBe("L0");
    expect(heldnessOf(record)).toBe("gone");
    // The reload reads the jar, presents the record once more (other-session
    // again — nothing revoked) and adopts the session it holds, under the
    // standing warning.
    const next = await aReloadComesTo();
    expect(next).toBe("signed-in");
    expect(screen.getByText("Sign-out not confirmed")).toBeTruthy();
    screen.getByRole("button", { name: "Sign out" }).click();
    await waitFor(() => expect(rows.get(elsewhere.token)?.revoked).toBe(true), { timeout: 8000 });
    const second = logoutsSent()[logoutsSent().length - 1]!;
    expect(second).toContain(`?lineage=${elsewhere.lineage}`);
    expect(storedRecord()).toBeNull();
  }, 40000);
});

describe("sign out everywhere, cleanup failed (the 503 signout_cleanup_failed contract)", () => {
  it("renders the actionable text, not the plain transport failure", async () => {
    window.history.replaceState({}, "", "/studio/account?sso_linked=github");
    cleanupFails = true;
    render(app());
    await screen.findByRole("button", { name: "Sign out everywhere" }, { timeout: 8000 });
    screen.getByRole("button", { name: "Sign out everywhere" }).click();
    const dialog = await screen.findByRole("dialog", {}, { timeout: 8000 });
    fireEvent.click(within(dialog).getByRole("button", { name: "Sign out everywhere" }));
    // The machine's own answer: the sign-out is done, the cleanup is not, and
    // the way on is named.
    await screen.findByText(
      "Sign-out done; cleanup incomplete — sign in again and repeat sign out everywhere.",
      {}, { timeout: 8000 },
    );
    expect(screen.queryByText(/API error 503/)).toBeNull();
  }, 30000);
});

// The session bound the /auth/me answer carries (session_lineage + expires_at)
// and the degraded wire that names no bound: the record's expiry is the read's
// bound, the notice names it, and where no read names one the record never
// expires — so the banner a delivery-resolved record stands under must offer
// the acknowledge, not a retry that would answer "none" without a wire call.
describe("the /auth/me bound and the degraded wire", () => {
  it("the /auth/me answer's bound lands in the record and in the notice", async () => {
    logoutMode = "500";
    meExpiresAt = "2027-06-01T00:00:00.000Z";
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    expect(storedRecord()?.expires_at).toBe("2027-06-01T00:00:00.000Z");
    expect(document.body.textContent).toContain("until 2027-06-01T00:00:00.000Z");
  }, 30000);

  it("today's wire names no bound: the delivery-resolved record stands and offers the acknowledge, not a dead retry", async () => {
    logoutMode = "500";
    meExpiresAt = undefined;
    // What the degraded path leaves: an in-app password change resolved the
    // record GONE (the stamp), the session the tab never saw a mint answer
    // for, so the record names no bound — no expiry rule will ever drop it.
    localStorage.setItem(
      SIGN_OUT_PENDING_KEY,
      JSON.stringify({
        state: "owed",
        lineage: "L0",
        account: "u",
        expires_at: null,
        since: Date.now() - 2000,
        abandoned_at: null,
        resolved_gone_at: Date.now() - 1000,
      }),
    );
    render(app());
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    // A GONE record cannot be presented: the retry is a no-op. The
    // acknowledge names the abandonment instead.
    expect(screen.queryByRole("button", { name: "Retry sign-out" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Got it — the session stays active" }));
    await screen.findByText("Sign-out abandoned — that session is still active", {}, { timeout: 8000 });
    // The notice stays — the record moved, it did not go (I2).
    expect(storedRecord()?.state).toBe("abandoned");
    expect(screen.queryByRole("button", { name: "Got it — the session stays active" })).toBeNull();
  }, 30000);
});
