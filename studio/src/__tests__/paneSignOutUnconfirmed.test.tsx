// @vitest-environment jsdom
// A desktop workspace pane's sign-out crosses the desktop's tunnel, which
// presents the connection's stored token and keeps it on any answer but a 2xx
// (cmd/iterion-desktop cloudRoundTripper.logout): such a sign-out ended
// nothing. The pane then says so and signs out again from there — it does not
// ask the shell to reconnect a session that is not over — and it keeps that
// pending sign-out to itself: a pane loaded again after the shell's re-login
// must not end the new session in its place, so nothing is written to the
// storage the shell and the other panes share.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
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
import { request } from "@/api/client";
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
let logoutStatus: number;

beforeEach(() => {
  localStorage.clear();
  calls = [];
  posted = [];
  logoutStatus = 500;
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
  vi.spyOn(window, "postMessage").mockImplementation((msg: unknown) => { posted.push(msg); });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/x/conn-1/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/x/conn-1/api/auth/me") return json(identity);
    // The cloud refused the desktop's session for this request.
    if (path === "/x/conn-1/api/probe") return json({ error: "authentication required" }, 401);
    // The tunnel's answer to the pane's sign-out: the cloud's own.
    if (path === "/x/conn-1/api/auth/logout") return logoutStatus === 204 ? new Response(null, { status: 204 }) : json({ error: "identity unavailable" }, logoutStatus);
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cleanup(); localStorage.clear(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe("a pane's sign-out the desktop did not confirm", () => {
  it("says so, keeps the pending sign-out to the pane, and asks the shell for nothing", async () => {
    expect(isScopedPane()).toBe(true);
    window.history.replaceState({}, "", "/x/conn-1/studio/runs/review-123");
    render(<Router base="/x/conn-1"><App /></Router>);
    await screen.findByRole("heading", { name: "Requested run review-123" });
    // A request the cloud refuses: the pane signs out.
    await request("/probe").catch(() => undefined);
    await screen.findByText("Sign-out not confirmed", {}, { timeout: 6000 });
    expect(screen.queryByText("Session expired")).toBeNull();
    expect(posted).toEqual([]);
    expect(Object.keys(localStorage)).toEqual([]);

    // Signed out again from the pane: confirmed this time. The session is
    // over, so the pane asks the shell to reconnect.
    logoutStatus = 204;
    screen.getByRole("button", { name: "Retry sign-out" }).click();
    await screen.findByText("Session expired");
    expect(screen.queryByText("Sign-out not confirmed")).toBeNull();
    await waitFor(() => expect(posted).toEqual([{ source: "iterion-pane", type: "auth-expired", connId: "conn-1" }]));
    expect(calls.filter((c) => c === "POST /x/conn-1/api/auth/logout")).toHaveLength(2);
  }, 15000);

  it("names the desktop as what holds it, since the pane's own mark dies with the document", async () => {
    window.history.replaceState({}, "", "/x/conn-1/studio/runs/review-123");
    render(<Router base="/x/conn-1"><App /></Router>);
    await screen.findByRole("heading", { name: "Requested run review-123" });
    await request("/probe").catch(() => undefined);
    const alert = await screen.findByRole("alert", {}, { timeout: 6000 });
    const text = alert.textContent ?? "";
    console.log("[pane] banner:", text);
    expect(text).toContain("Sign-out not confirmed");
    // The pane cannot keep the browser's promise: its mark is module state and
    // nothing of it is in the storage the shell shares (asserted above). What
    // keeps the session unused is the desktop, which marks the connection's
    // jar sign-out-pending and re-presents the logout itself — so that is what
    // the sentence says.
    expect(text).not.toContain("iterion signs no one back into it");
    expect(text).toContain("The desktop has stopped using it and keeps presenting the sign-out until the cloud confirms");
    expect(text).toContain("retry before you leave");
  }, 15000);
});
