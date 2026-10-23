// @vitest-environment jsdom
// A pending sign-out names the session it is about. The mark carries the
// lineage /auth/me answered (session_lineage), the logout presents it
// (?lineage=<id>), and the server ends THAT lineage or nothing: a 204 carrying
// X-Iterion-Logout: other-session revoked nothing, because the refresh token
// the browser presented belongs to another session — one delivered since, by a
// route no studio code ran for (an SSO callback). The mark is then moot, and
// the session the browser does hold is its own to read.
//
// What a mark NAMES decides whether this page may hand the browser to an
// identity provider. It names a lineage: the landing presents that logout with
// the provider's cookies, the server ends that lineage or nothing, and the
// delivered session is adopted — the sign-in completes, so the start goes. It
// names none: the landing's logout would end the session the provider just
// delivered, so the start is held, with the reason and the way on.
//
// The HTTP boundary is modelled as in signOutWindow.test.tsx: the jar is read
// when a request is SENT and moved when it ARRIVES.
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { useRoute } from "wouter";

// The shell's chrome, reduced to the one control the account chip
// (UserTeamChip) wires to the same context action an operator clicks.
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
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => <h1>Restricted account</h1> }));
vi.mock("@/views/ConfigShare", () => ({ default: () => null }));
vi.mock("@/views/Marketplace", () => ({ default: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: function RequestedRun() {
  const [, params] = useRoute("/runs/:id");
  return <h1>Requested run {params?.id}</h1>;
} }));

import App from "@/App";
import { cancelScheduledRefresh, SIGN_OUT_PENDING_KEY } from "@/api/client";
import { heldnessOf } from "@/api/signOutMachine";
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
let meStoreDown: boolean;
let logoutMode: "ok" | "500";
let log: string[];
let navigations: string[];
let offJsdomError: (() => void) | null = null;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const shell = () => screen.queryByRole("heading", { name: /Requested run|Signed-in home/ });
const marked = () => localStorage.getItem(SIGN_OUT_PENDING_KEY);
const logoutsSent = () => log.filter((l) => l.startsWith("POST /api/auth/logout"));
const alerts = () => Array.from(document.querySelectorAll("[role=alert]")).map((n) => n.textContent ?? "");

// The SSO callback: the cloud delivers a session of its own lineage and sets
// its cookies on the browser — no studio code runs before the landing page.
function ssoCallbackDeliversASession(n: number): string {
  rows.set(`T${n}`, { lineage: `SSO${n}`, revoked: false });
  liveAccess.add(`A${n}`);
  jar = { refresh: `T${n}`, access: `A${n}` };
  return `T${n}`;
}

beforeEach(() => {
  localStorage.clear();
  jar = { refresh: "T0", access: "A0" };
  liveAccess = new Set(["A0"]);
  rows = new Map([["T0", { lineage: "L0", revoked: false }]]);
  nameSession = true;
  meStoreDown = false;
  logoutMode = "ok";
  log = [];
  navigations = [];
  window.history.replaceState({}, "", "/studio/runs/review-123");
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
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
    if (path === "/api/auth/me") {
      if (!sent.access || !liveAccess.has(sent.access)) return json({ error: "authentication required" }, 401);
      if (meStoreDown) return json({ error: "identity unavailable" }, 500);
      const lineage = sent.refresh ? rows.get(sent.refresh)?.lineage : undefined;
      return json(nameSession && lineage ? { ...account, session_lineage: lineage } : account);
    }
    if (path === "/api/auth/logout") {
      if (logoutMode === "500") return json({ error: "identity unavailable" }, 500);
      const asked = url.searchParams.get("lineage");
      const held = sent.refresh ? rows.get(sent.refresh)?.lineage : undefined;
      if (asked && held && asked !== held) {
        // S2: the presented refresh token belongs to another lineage. Nothing
        // is revoked, the cookies are kept, and the header says so.
        log.push(`  logout: ${asked} is not this browser's session (${held}); nothing revoked`);
        return new Response(null, { status: 204, headers: { "X-Iterion-Logout": "other-session" } });
      }
      for (const r of rows.values()) if (held && r.lineage === held) r.revoked = true;
      jar = { refresh: null, access: null };
      log.push(`  logout: lineage ${held ?? "none"} revoked, jar cleared`);
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

// Signs out of a session whose identity the studio has READ — the ordinary
// case: the operator is looking at their account when they click.
async function signOutOfTheShell() {
  render(<App />);
  await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
  screen.getByRole("button", { name: "Sign out" }).click();
}

describe("a pending sign-out is bound to the session it was set for", () => {
  it("presents the lineage /auth/me named, and a 204 without the header ends that session", async () => {
    await signOutOfTheShell();
    await waitFor(() => expect(logoutsSent()).toHaveLength(1), { timeout: 8000 });
    console.log("[bound] the logout presented:", JSON.stringify(logoutsSent()));
    expect(logoutsSent()[0]).toContain("?lineage=L0");
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(marked()).toBeNull();
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
  }, 20000);

  it("names no lineage when the credential named no session, and ends what the browser holds", async () => {
    nameSession = false;
    await signOutOfTheShell();
    await waitFor(() => expect(logoutsSent()).toHaveLength(1), { timeout: 8000 });
    console.log("[unnamed] the logout presented:", JSON.stringify(logoutsSent()));
    expect(logoutsSent()[0]).not.toContain("lineage=");
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    expect(rows.get("T0")?.revoked).toBe(true);
    expect(marked()).toBeNull();
  }, 20000);

  it("holds the SSO start while the unconfirmed mark names a session, and goes on the explicit anyway", async () => {
    logoutMode = "500";
    await signOutOfTheShell();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    expect(marked()).not.toBeNull();

    const before = logoutsSent().length;
    (await screen.findByRole("button", { name: "Continue with GitHub" })).click();
    // The click presents the logout again; it still fails — and the start is
    // HELD: this browser still holds the named session, and a landing here
    // would deliver a new one the sign-out never got to end (r7c F2, I4).
    await new Promise((r) => setTimeout(r, 300));
    console.log("[sso-named] navigations:", navigations.length, "| logouts:", JSON.stringify(logoutsSent()), "| alerts:", JSON.stringify(alerts()));
    expect(navigations).toEqual([]);
    expect(logoutsSent().length).toBe(before + 1);
    expect(logoutsSent()[before]).toContain("?lineage=L0");
    expect(screen.getByRole("button", { name: "Continue with GitHub anyway" })).toBeTruthy();

    // The way on: the record moves to ABANDONED — it STANDS, with its lineage,
    // so the notice survives in every tab (I2) — and the redirect goes.
    screen.getByRole("button", { name: "Continue with GitHub anyway" }).click();
    await waitFor(() => expect(navigations.length).toBe(1), { timeout: 8000 });
    const record = JSON.parse(marked()!);
    expect(record.state).toBe("abandoned");
    expect(record.lineage).toBe("L0");
    expect(rows.get("T0")?.revoked).toBe(false);
  }, 30000);

  it("holds the SSO start while the unconfirmed mark names no session, says why, and offers the way on", async () => {
    nameSession = false;
    logoutMode = "500";
    await signOutOfTheShell();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    expect(marked()).not.toBeNull();

    (await screen.findByRole("button", { name: "Continue with GitHub" })).click();
    await waitFor(() => expect(alerts().some((t) => t.includes("signing in with GitHub is on hold"))).toBe(true), { timeout: 8000 });
    await new Promise((r) => setTimeout(r, 300));
    console.log("[sso-unnamed] navigations:", navigations.length, "| alerts:", JSON.stringify(alerts()));
    // Held, and the page says what holds it: a mark about whatever session the
    // browser holds would end the one GitHub is about to deliver.
    expect(navigations).toEqual([]);
    expect(screen.queryByText("Redirecting to your provider…")).toBeNull();
    expect(screen.getByRole("button", { name: "Retry sign-out" })).toBeTruthy();

    // The way on, which says what it costs: the sign-out is abandoned — the
    // record STANDS as abandoned (the notice survives; nothing is dropped
    // silently, I2).
    screen.getByRole("button", { name: "Continue with GitHub anyway" }).click();
    await waitFor(() => expect(navigations.length).toBe(1), { timeout: 8000 });
    console.log("[sso-unnamed] after the way on: mark", marked(), "| T0 revoked:", rows.get("T0")?.revoked);
    const record = JSON.parse(marked()!);
    expect(record.state).toBe("abandoned");
  }, 30000);

  it("redirects to the provider when no sign-out is pending", async () => {
    jar = { refresh: null, access: null };
    render(<App />);
    (await screen.findByRole("button", { name: "Continue with GitHub" }, { timeout: 8000 })).click();
    await waitFor(() => expect(navigations.length).toBe(1), { timeout: 8000 });
    console.log("[sso-clean] logouts presented:", JSON.stringify(logoutsSent()));
    expect(logoutsSent()).toEqual([]);
  }, 20000);

  it("keeps a session delivered while the mark stood: the logout revokes nothing and that session is adopted", async () => {
    logoutMode = "500";
    await signOutOfTheShell();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });
    expect(marked()).not.toBeNull();

    // A session reaches this browser anyway — an SSO callback, whose cookies
    // no studio code is running for — and the logout endpoint recovers.
    const sso = ssoCallbackDeliversASession(1);
    logoutMode = "ok";
    cleanup();
    const landingAt = log.length;
    render(<App />);
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    console.log("[other-session] landing:", JSON.stringify(log.slice(landingAt)));
    console.log("[other-session] SSO session", sso, "revoked:", rows.get(sso)?.revoked, "| T0 revoked:", rows.get("T0")?.revoked);
    // The session the user asked to end was not this browser's any more, so
    // the logout ended nothing — and the one it holds was not ended in its
    // place.
    expect(rows.get(sso)?.revoked).toBe(false);
    expect(rows.get("T0")?.revoked).toBe(false);
    expect(jar.refresh).toBe(sso);
    // The record STANDS, resolved GONE: the sign-out of L0 was never
    // confirmed, and the presentation's other-session answer is its
    // resolution — the warning persists over the adopted session (I1, I2).
    const record = JSON.parse(marked()!);
    expect(record.state).toBe("owed");
    expect(record.lineage).toBe("L0");
    expect(log.slice(landingAt).some((l) => l.startsWith("POST /api/auth/logout?lineage=L0"))).toBe(true);
    expect(log.slice(landingAt).some((l) => l.startsWith("GET /api/auth/me"))).toBe(true);
    expect(shell()).not.toBeNull();
    expect(screen.getByText("Sign-out not confirmed")).toBeTruthy();
  }, 30000);

  it("retried from the banner, a logout that revoked nothing adopts the session the browser holds", async () => {
    logoutMode = "500";
    await signOutOfTheShell();
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 8000 });

    const sso = ssoCallbackDeliversASession(2);
    logoutMode = "ok";
    screen.getByRole("button", { name: "Retry sign-out" }).click();
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
    console.log("[other-session/retry] SSO session", sso, "revoked:", rows.get(sso)?.revoked, "| mark:", marked());
    expect(rows.get(sso)?.revoked).toBe(false);
    // The record stands resolved GONE (nothing revoked, nothing dropped): the
    // adopted session is this browser's own to use, under the warning.
    const record = JSON.parse(marked()!);
    expect(record.state).toBe("owed");
    expect(heldnessOf(record)).toBe("gone");
    expect(screen.getByText("Sign-out not confirmed")).toBeTruthy();
  }, 30000);
});
