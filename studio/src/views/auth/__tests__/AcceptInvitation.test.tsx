// @vitest-environment jsdom
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// The real App, AuthProvider and invitation view; only the destination views
// and the HTTP boundary are replaced. An accept the server committed is
// reported as accepted, whatever the identity re-read after it answers.
vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => <h1>Restricted account</h1> }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/views/teams/TeamPage", () => ({ default: () => <h1>Team page</h1> }));

import App from "@/App";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const user = { id: "u", email: "r@example.test", status: "active", is_super_admin: false };
const identity = {
  user,
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};
let calls: string[];
let meStatus: number;
let accepted: boolean;

beforeEach(() => {
  calls = [];
  meStatus = 200;
  accepted = false;
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") return meStatus === 200 ? json(identity) : json({ error: "identity unavailable" }, meStatus);
    if (path === "/api/auth/invitations/lookup") {
      return accepted ? json({ error: "identity: invitation already accepted" }, 409) : json({ email: "r@example.test", role: "member", team_id: "t9", team_name: "Nine" });
    }
    if (path === "/api/auth/invitations/accept") {
      if (accepted) return json({ error: "identity: invitation already accepted" }, 409);
      accepted = true;
      return json({ user_id: "u", team_id: "t9", role: "member" });
    }
    if (path === "/api/auth/me/team/t9") return json({ user, active_org_id: "o9", active_team_id: "t9", active_role: "member" });
    return json({}, 200);
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("accepting an invitation", () => {
  it("lands on the team it joined when the identity re-read after the accept fails", async () => {
    window.history.replaceState({}, "", "/invitations/accept?token=inv-1");
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><App /></QueryClientProvider>);
    const join = await screen.findByRole("button", { name: "Join Nine" }, { timeout: 6000 });
    meStatus = 500;
    fireEvent.click(join);
    await waitFor(() => expect(calls).toContain("POST /api/auth/invitations/accept"));
    await screen.findByRole("heading", { name: "Team page" }, { timeout: 8000 });
    expect(window.location.pathname).toBe("/studio/teams/t9");
    expect(screen.queryByText(/identity unavailable/i)).toBeNull();
    expect(accepted).toBe(true);
  }, 15000);
});
