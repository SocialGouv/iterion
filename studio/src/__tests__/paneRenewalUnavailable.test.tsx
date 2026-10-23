// @vitest-environment jsdom
// A desktop workspace pane whose access token expired while the desktop could
// not renew it: the desktop's tunnel (cmd/iterion-desktop cloudRoundTripper)
// answers such a request 503 "session renewal unavailable" — only a renewal
// the cloud REFUSED passes the 401 through. The pane must treat the 503 as
// the transient failure it is: no sign-out, no re-login asked of the shell.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Router, useRoute } from "wouter";

vi.hoisted(() => {
  (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__ = "/x/conn-1";
});

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
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: function RequestedRun() {
  const [, params] = useRoute("/runs/:id");
  return <h1>Requested run {params?.id}</h1>;
} }));

import App from "@/App";
import { ApiError, request } from "@/api/client";
import { isScopedPane } from "@/lib/scope";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const identity = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};
let calls: string[];
let posted: unknown[];

beforeEach(() => {
  calls = [];
  posted = [];
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
  vi.spyOn(window, "postMessage").mockImplementation((msg: unknown) => { posted.push(msg); });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/x/conn-1/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/x/conn-1/api/auth/me") return json(identity);
    if (path === "/x/conn-1/api/probe") return json({ error: "session renewal unavailable" }, 503);
    if (path === "/x/conn-1/api/auth/logout") return new Response(null, { status: 204 });
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe("a pane whose renewal the desktop could not serve", () => {
  it("fails the request as transient and keeps the pane signed in", async () => {
    expect(isScopedPane()).toBe(true);
    window.history.replaceState({}, "", "/x/conn-1/studio/runs/review-123");
    render(<Router base="/x/conn-1"><App /></Router>);
    await screen.findByRole("heading", { name: "Requested run review-123" });
    const err = await request("/probe").then(() => null, (e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(503);
    await new Promise((r) => setTimeout(r, 200));
    expect(calls).not.toContain("POST /x/conn-1/api/auth/logout");
    expect(posted).toEqual([]);
    expect(screen.queryByText("Session expired")).toBeNull();
    expect(screen.getByRole("heading", { name: "Requested run review-123" })).toBeTruthy();
  });
});
