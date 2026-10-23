// @vitest-environment jsdom
// "Sign out everywhere" is the compromise response: it ends every refresh
// session of the account, revokes every personal access token it has, and
// disconnects every SSO identity a signed-in session connected from the
// account page — the identity the account signed up or first signed in with
// stays, so SSO sign-in still works. A CLI that stops working, a CI job that
// starts failing and a connection gone from the list are what the operator
// finds afterwards, so the page and its confirmation say it BEFORE the click —
// "PATs are not affected" sent them looking for another cause.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";

vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/ChatDock/ChatDock", () => ({ default: () => null, AssistantDockCrashed: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => null, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: () => null }));

import App from "@/App";
import { cancelScheduledRefresh } from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const account = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
  session_lineage: "L0",
};

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

beforeEach(() => {
  localStorage.clear();
  window.history.replaceState({}, "", "/studio/account?sso_linked=github");
  useServerInfoStore.setState({ info: { mode: "cloud", auth_required: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") return json(account);
    if (path === "/api/me/sso/links") return json({ links: [] });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (path.startsWith("/api/")) return json({});
    throw new Error(`unexpected ${path}`);
  }));
});

afterEach(() => {
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("the account page's sign-out-everywhere", () => {
  it("says the account's personal access tokens and its connected SSO identities go with the sessions, in the section and in the confirmation", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><App /></QueryClientProvider>);
    const button = await screen.findByRole("button", { name: "Sign out everywhere" }, { timeout: 8000 });

    const section = button.closest("section");
    const blurb = section?.textContent ?? "";
    console.log("[revoke-all] section:", JSON.stringify(blurb));
    expect(blurb).toContain("every personal access token of this account is revoked");
    expect(blurb).toContain("CLI and CI tokens included");
    // The sentence this replaces was the opposite of the truth.
    expect(blurb).not.toContain("PATs are not affected");
    // The SSO identities connected from this page go too, and the one the
    // account signs in with does not — an SSO-only account is not locked out.
    expect(blurb).toContain("Every SSO identity you connected from this page is disconnected");
    expect(blurb).toContain("the one you signed up or first signed in with stays");

    button.click();
    const dialog = await screen.findByRole("dialog", {}, { timeout: 8000 });
    console.log("[revoke-all] confirmation:", JSON.stringify(dialog.textContent));
    expect(dialog.textContent).toContain("End every signed-in session for this account AND revoke its personal access tokens");
    expect(dialog.textContent).toContain("CI jobs use will stop working until you issue new ones");
    expect(dialog.textContent).toContain("Every SSO identity you connected from this page is disconnected too");
    expect(dialog.textContent).toContain("the one you signed up or first signed in with stays");
  }, 30000);
});
