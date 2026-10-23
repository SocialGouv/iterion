// @vitest-environment jsdom
// A sign-out the user asked for ends the session this browser HOLDS.
//
// The session can have moved since the identity read the mark's lineage comes
// from: the in-app password change answers a new one and sets its cookies, and
// so does another tab's sign-in, a reset or an SSO landing. The logout then
// names a lineage the server does not recognise as this browser's — 204,
// X-Iterion-Logout: other-session, nothing revoked, cookies kept — and the
// sign-out is NOT over: it is presented again naming no lineage, and the
// state is what that answer came to. A browser left on the sign-in page with
// a live session is the next person's session.
//
// The HTTP boundary is modelled as in signOutBoundToItsSession.test.tsx: the
// jar is read when a request is SENT and moved when it ARRIVES.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// The shell's chrome, reduced to the one control the account chip
// (UserTeamChip) wires to the same context action an operator clicks.
vi.mock("@/components/shared/AppShell", async () => {
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
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: () => null }));

import App from "@/App";
import { cancelScheduledRefresh, SIGN_OUT_PENDING_KEY } from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const account = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};

interface Row { lineage: string; revoked: boolean }

let jar: { refresh: string | null; access: string | null };
let liveAccess: Set<string>;
let rows: Map<string, Row>;
// nameSession = false models what answers no session_lineage: a personal
// access token, an access token minted before sessions carried a lineage.
let nameSession: boolean;
let logoutMode: "ok" | "500" | "always-other-session";
let log: string[];
let nextSession = 1;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const shell = () => screen.queryByRole("heading", { name: /Signed-in home/ });
const marked = () => localStorage.getItem(SIGN_OUT_PENDING_KEY);
const logoutsSent = () => log.filter((l) => l.startsWith("POST /api/auth/logout"));
const app = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <App />
  </QueryClientProvider>
);

// serverOpensASession is a session the server opens for this browser and whose
// cookies it sets: a credential route's answer (the in-app password change), or
// one delivered to another tab of the same browser — the jar is shared.
function serverOpensASession(): { token: string; lineage: string } {
  const n = nextSession++;
  rows.set(`T${n}`, { lineage: `L${n}`, revoked: false });
  liveAccess.add(`A${n}`);
  jar = { refresh: `T${n}`, access: `A${n}` };
  return { token: `T${n}`, lineage: `L${n}` };
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
  window.history.replaceState({}, "", "/studio/account?sso_linked=github");
  useServerInfoStore.setState({ info: { mode: "cloud", auth_required: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), "https://iterion.invalid");
    const path = url.pathname;
    const method = init?.method ?? "GET";
    const sent = { ...jar };
    log.push(`${method} ${path}${url.search} [refresh=${sent.refresh ?? "none"} access=${sent.access ?? "none"}]`);
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (path === "/api/me/sso/links") return json({ links: [] });
    if (path === "/api/auth/me") {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "authentication required" }, 401);
      const lineage = sent.refresh ? rows.get(sent.refresh)?.lineage : undefined;
      return json(nameSession && lineage ? { ...account, session_lineage: lineage } : account);
    }
    if (path === "/api/me/password" && method === "POST") {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "token expired" }, 401);
      // The change revokes every session of the account and opens the caller a
      // NEW one (a fresh root id = a new lineage), whose cookies it sets.
      for (const r of rows.values()) r.revoked = true;
      liveAccess.clear();
      const s = serverOpensASession();
      log.push(`  password change: every session revoked, new session ${s.token} (${s.lineage}) set`);
      return json({ user: account.user, active_org_id: "o1", active_team_id: "t1", active_role: "member" });
    }
    if (path === "/api/me/sessions/revoke-all" && method === "POST") {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "token expired" }, 401);
      // Sign out everywhere: every session of the account ends, this one
      // included, and the caller's own cookies are cleared.
      for (const r of rows.values()) r.revoked = true;
      liveAccess.clear();
      jar = { refresh: null, access: null };
      log.push("  revoke-all: every session revoked, this browser's cookies cleared");
      return new Response(null, { status: 204 });
    }
    if (path === "/api/auth/logout") {
      if (logoutMode === "500") return json({ error: "identity unavailable" }, 500);
      const asked = url.searchParams.get("lineage");
      const held = sent.refresh ? rows.get(sent.refresh)?.lineage : undefined;
      if (logoutMode === "always-other-session") {
        // A server contradicting its own contract: a logout that named no
        // lineage ends the session presented, or there is none — it can never
        // be about another one.
        log.push(`  logout: answered other-session for lineage ${asked ?? "none"}; nothing revoked`);
        return new Response(null, { status: 204, headers: { "X-Iterion-Logout": "other-session" } });
      }
      if (asked && held && asked !== held) {
        // S2: the presented refresh token belongs to another lineage. Nothing
        // is revoked, the cookies are kept, and the header says so.
        log.push(`  logout: ${asked} is not this browser's session (${held}); nothing revoked, cookies kept`);
        return new Response(null, { status: 204, headers: { "X-Iterion-Logout": "other-session" } });
      }
      for (const r of rows.values()) if (held && r.lineage === held) r.revoked = true;
      jar = { refresh: null, access: null };
      log.push(`  logout: lineage ${held ?? "none"} revoked, jar cleared`);
      return new Response(null, { status: 204 });
    }
    if (path === "/api/auth/refresh") {
      const row = sent.refresh ? rows.get(sent.refresh) : undefined;
      if (!row || row.revoked) return json({ error: "session revoked" }, 401);
      return json({ error: "unexpected refresh in this test" }, 500);
    }
    if (path.startsWith("/api/")) {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "token expired" }, 401);
      return json({});
    }
    throw new Error(`unexpected ${method} ${path}`);
  }));
});

afterEach(async () => {
  await new Promise((r) => setTimeout(r, 10));
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function accountPageUp() {
  render(app());
  await screen.findByRole("button", { name: "Change password" }, { timeout: 8000 });
}

// What the next person at this computer comes to — a reload, a new tab.
async function aNewTabComesTo(): Promise<"signed-in" | "signed-out"> {
  cleanup();
  render(app());
  await waitFor(() => {
    if (
      !shell() &&
      !screen.queryByRole("button", { name: "Change password" }) &&
      !screen.queryByRole("heading", { name: "Sign in to iterion" }) &&
      // The account page can land on a tab without the password form; the
      // tablist itself is the signed-in marker then.
      !screen.queryByRole("tab", { name: /API keys/ })
    ) {
      throw new Error("not settled");
    }
  }, { timeout: 8000 });
  return screen.queryByRole("heading", { name: "Sign in to iterion" }) ? "signed-out" : "signed-in";
}

async function changeThePassword() {
  fireEvent.change(screen.getByPlaceholderText("Current password"), { target: { value: "old-password-1" } });
  fireEvent.change(screen.getByPlaceholderText("New password (≥ 8 characters)"), { target: { value: "new-password-2" } });
  fireEvent.change(screen.getByPlaceholderText("Confirm new password"), { target: { value: "new-password-2" } });
  fireEvent.click(screen.getByRole("button", { name: "Change password" }));
  await screen.findByText("Password updated.", {}, { timeout: 8000 });
}

describe("a sign-out after the session the browser holds was replaced", () => {
  it("ends the session the in-app password change delivered, not the one it replaced", async () => {
    await accountPageUp();
    await changeThePassword();
    const delivered = String(jar.refresh);
    console.log("[change] the change delivered:", JSON.stringify(jar));

    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    console.log("[change] logouts:", JSON.stringify(logoutsSent()), "| jar:", JSON.stringify(jar));
    // The logout named the session the browser holds — the delivered one — and
    // ended it. One presentation: it was the right session the first time.
    expect(logoutsSent()).toHaveLength(1);
    expect(logoutsSent()[0]).toContain("?lineage=L1");
    expect(rows.get(delivered)?.revoked).toBe(true);
    expect(jar).toEqual({ refresh: null, access: null });
    expect(marked()).toBeNull();
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
    expect(await aNewTabComesTo()).toBe("signed-out");
  }, 40000);

  it("ends nothing but the session it names when another tab's session replaced it, and warns honestly", async () => {
    await accountPageUp();
    // Another tab's sign-in (or an SSO landing): the shared jar moves to a new
    // lineage. THIS tab read its identity before that, so the lineage it knows
    // is the replaced one.
    //
    // (The storage dispatch this witness once made for the renewal stamp was
    // the phantom listener's vacuous witness — no code listens to that key
    // (r7c S1); the real cross-tab witness is the record's storage event in
    // signOutMachineTransitions.test.tsx.)
    const elsewhere = serverOpensASession();

    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    console.log("[cross-tab] logouts:", JSON.stringify(logoutsSent()));
    console.log("[cross-tab] session", elsewhere.token, "revoked:", rows.get(elsewhere.token)?.revoked, "| jar:", JSON.stringify(jar), "| mark:", marked());
    // The machine's SIGN-OUT REQUESTED has ONE presentation, naming the
    // lineage the record was written with: the server ends that lineage or
    // nothing. There is no unnamed re-ask — ending the session that replaced
    // the named one is a NEW ask, the user's second click, naming what the
    // next identity read holds.
    expect(logoutsSent()).toHaveLength(1);
    expect(logoutsSent()[0]).toContain("?lineage=L0");
    expect(rows.get(elsewhere.token)?.revoked).toBe(false);
    expect(jar).toEqual({ refresh: `T1`, access: "A1" });
    // The record stands, resolved GONE by the other-session answer: the sign-
    // out of L0 was never confirmed and is not silently dropped (I2) — the
    // warning says the session this browser holds is not the one it names.
    const record = JSON.parse(marked()!);
    expect(record.state).toBe("owed");
    expect(record.lineage).toBe("L0");
    expect(screen.getByText("Sign-out not confirmed")).toBeTruthy();
    // A new tab reads the record, resolves it GONE (the presentation answers
    // other-session again) and adopts the session the browser holds — under
    // the standing warning, never as a plain sign-in.
    expect(await aNewTabComesTo()).toBe("signed-in");
    expect(screen.getByText("Sign-out not confirmed")).toBeTruthy();
  }, 40000);

  it("presents one logout for sign-out everywhere, whose cookies the server already cleared", async () => {
    await accountPageUp();
    screen.getByRole("button", { name: "Sign out everywhere" }).click();
    const dialog = await screen.findByRole("dialog", {}, { timeout: 8000 });
    fireEvent.click(within(dialog).getByRole("button", { name: "Sign out everywhere" }));
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    console.log("[everywhere] log:", JSON.stringify(log.filter((l) => l.includes("revoke-all") || l.startsWith("POST /api/auth/logout") || l.startsWith("  logout"))));
    // The revoke-all cleared this browser's cookies, so the logout presents
    // none: the server ends nothing more and says nothing about another
    // session, and no second presentation follows.
    expect(logoutsSent()).toHaveLength(1);
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(marked()).toBeNull();
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
    expect(await aNewTabComesTo()).toBe("signed-out");
  }, 40000);

  it("claims nothing when a logout naming no session answers that it revoked nothing", async () => {
    // The credential named no session, so the mark names none either: the
    // logout is about whatever the browser holds. A 204 saying it revoked
    // nothing is then the server contradicting itself — nothing is signed out,
    // and the warning says the session may still be live.
    nameSession = false;
    logoutMode = "always-other-session";
    await accountPageUp();
    screen.getByRole("button", { name: "Sign out" }).click();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    console.log("[unnamed] logouts:", JSON.stringify(logoutsSent()), "| mark:", marked(), "| T0 revoked:", rows.get("T0")?.revoked);
    expect(logoutsSent()).toHaveLength(1);
    expect(logoutsSent()[0]).not.toContain("lineage=");
    expect(rows.get("T0")?.revoked).toBe(false);
    expect(marked()).not.toBeNull();
  }, 40000);
});
