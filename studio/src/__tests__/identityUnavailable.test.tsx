// @vitest-environment jsdom
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { useRoute } from "wouter";

// The real App, AuthProvider and api client; only the destination views and
// the HTTP boundary are replaced. What the shell shows when the server
// cannot say who a live session belongs to, and when it cannot renew it.
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
import { cancelScheduledRefresh, request, SessionRenewalUnavailableError } from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const identity = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};
let calls: string[];
let meStatus: number;
let refreshStatus: number;
let renewed: boolean;

beforeEach(() => {
  calls = [];
  meStatus = 200;
  refreshStatus = 200;
  renewed = false;
  window.history.replaceState({}, "", "/studio/runs/review-123");
  useServerInfoStore.setState({ info: { mode: "cloud", marketplace_enabled: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud", marketplace_enabled: true });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (path === "/api/auth/me") return meStatus === 200 ? json(identity) : json({ error: "identity unavailable" }, meStatus);
    if (path === "/api/probe") return renewed ? json({ ok: true }) : json({ error: "access token expired" }, 401);
    if (path === "/api/auth/refresh") {
      if (refreshStatus !== 200) return json({ error: refreshStatus === 401 ? "auth: session revoked" : "identity unavailable" }, refreshStatus);
      renewed = true;
      return json({ user: identity.user, active_org_id: "o1", active_team_id: "t1", active_role: "member" });
    }
    if (path === "/api/auth/logout") return new Response(null, { status: 204 });
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cancelScheduledRefresh(); cleanup(); vi.unstubAllGlobals(); });

describe("a live session whose identity the server cannot read", () => {
  it("shows identity unavailable with a retry — not the sign-in form, not a restricted account", async () => {
    meStatus = 500;
    render(<App />);
    await screen.findByText("Your identity is unavailable", {}, { timeout: 6000 });
    expect(screen.getByRole("button", { name: /retry/i })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Sign in to iterion" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Restricted account" })).toBeNull();
    expect(calls).not.toContain("POST /api/auth/logout");

    // The server reads again: the retry lands on the page that was asked for.
    meStatus = 200;
    await screen.findByRole("heading", { name: "Requested run review-123" }, { timeout: 8000 });
  }, 20000);

  // The doors that do not rest on the session's identity stay open while it
  // cannot be read: a password link and a config-share link authenticate on
  // the token in their URL, /login signs in as another account, the
  // marketplace is public.
  for (const [url, door] of [
    ["/auth/reset?token=iar_reset-token-1", "Reset your password"],
    ["/config/share-1#token=cs_abc", "Config share editor"],
    ["/login", "Sign in to iterion"],
    ["/studio/marketplace", "Public marketplace"],
  ] as const) {
    it(`opens ${url.split(/[?#]/)[0]} while the identity cannot be read`, async () => {
      meStatus = 500;
      window.history.replaceState({}, "", url);
      render(<App />);
      await screen.findByRole("heading", { name: door }, { timeout: 6000 });
      expect(screen.queryByText("Your identity is unavailable")).toBeNull();
    }, 15000);
  }

  it("offers a way out: signing out ends the state and lands on the sign-in", async () => {
    meStatus = 500;
    render(<App />);
    await screen.findByText("Your identity is unavailable", {}, { timeout: 6000 });
    screen.getByRole("button", { name: "Sign out" }).click();
    await waitFor(() => expect(calls).toContain("POST /api/auth/logout"));
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 6000 });
  }, 15000);
});

describe("the silent refresh of an expired access token", () => {
  it("answered 500: the request fails as transient, the session is neither signed out nor revoked", async () => {
    refreshStatus = 500;
    render(<App />);
    await screen.findByRole("heading", { name: "Requested run review-123" });
    await expect(request("/probe")).rejects.toBeInstanceOf(SessionRenewalUnavailableError);
    await new Promise((r) => setTimeout(r, 200));
    expect(calls).not.toContain("POST /api/auth/logout");
    expect(screen.getByRole("heading", { name: "Requested run review-123" })).toBeTruthy();

    // The server serves again: the next request renews the same session.
    refreshStatus = 200;
    await expect(request("/probe")).resolves.toEqual({ ok: true });
    expect(calls).not.toContain("POST /api/auth/logout");
  });

  it("answered 401: the session is over, the shell signs out", async () => {
    refreshStatus = 401;
    render(<App />);
    await screen.findByRole("heading", { name: "Requested run review-123" });
    await expect(request("/probe")).rejects.toThrow();
    await waitFor(() => expect(calls).toContain("POST /api/auth/logout"));
    await screen.findByRole("heading", { name: "Sign in to iterion" });
  });
});
