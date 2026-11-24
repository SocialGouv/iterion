// @vitest-environment jsdom
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { useParams } from "wouter";

// The operator's legal pages (ADR-160) answer at one root address,
// /legal/<slug>, whatever the session: an anonymous visitor, a signed-in
// operator (never carried under /studio) and a session whose identity the
// server cannot read all open the same page. The real App, AuthProvider and
// api client; only the destination views and the HTTP boundary are replaced.
vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => <h1>Restricted account</h1> }));
vi.mock("@/views/Marketplace", () => ({ default: () => <h1>Public marketplace</h1> }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/views/Legal", () => ({ default: function LegalStub() {
  const { slug } = useParams<{ slug: string }>();
  return <h1>Legal page {slug}</h1>;
} }));

import App from "@/App";
import { cancelScheduledRefresh } from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const identity = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};
let meStatus: number;

beforeEach(() => {
  meStatus = 200;
  useServerInfoStore.setState({ info: { mode: "cloud", marketplace_enabled: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud", marketplace_enabled: true });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (path === "/api/auth/me") return meStatus === 200 ? json(identity) : json({ error: "no session" }, meStatus);
    if (path === "/api/auth/refresh") return json({ error: "auth: no session" }, 401);
    if (path === "/api/auth/logout") return new Response(null, { status: 204 });
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cancelScheduledRefresh(); cleanup(); vi.unstubAllGlobals(); });

describe("the legal pages answer at their root address", () => {
  for (const [who, status] of [
    ["an anonymous visitor", 401],
    ["a signed-in operator", 200],
    ["a session whose identity cannot be read", 500],
  ] as const) {
    it(`opens /legal/cgu for ${who}`, async () => {
      meStatus = status;
      window.history.replaceState({}, "", "/legal/cgu");
      render(<App />);
      await screen.findByRole("heading", { name: "Legal page cgu" }, { timeout: 6000 });
      expect(window.location.pathname).toBe("/legal/cgu");
      expect(screen.queryByText("Your identity is unavailable")).toBeNull();
      expect(screen.queryByRole("heading", { name: "Sign in to iterion" })).toBeNull();
    }, 15000);
  }

  // An address under /legal/ that names no document still opens the page
  // (its own "not available" answer), never a blank screen — for every
  // session that reaches LegalDoor.
  for (const [who, status] of [
    ["a signed-in operator", 200],
    ["a session whose identity cannot be read", 500],
  ] as const) {
    for (const path of ["/legal/", "/legal/a/b"]) {
      it(`opens the page at ${path} for ${who}`, async () => {
        meStatus = status;
        window.history.replaceState({}, "", path);
        render(<App />);
        await screen.findByRole("heading", { name: /^Legal page\s*$/ }, { timeout: 6000 });
        expect(window.location.pathname).toBe(path);
      }, 15000);
    }
  }

  // /legal with no document is a root side door too: a signed-in operator
  // lands on the studio home, never on a /studio/legal the studio does not
  // know.
  it("carries a signed-in operator from a bare /legal to the studio home", async () => {
    window.history.replaceState({}, "", "/legal");
    render(<App />);
    await screen.findByRole("heading", { name: "Signed-in home" }, { timeout: 6000 });
    expect(window.location.pathname).toBe("/studio");
  }, 15000);
});
