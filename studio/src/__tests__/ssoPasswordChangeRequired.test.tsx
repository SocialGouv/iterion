// @vitest-environment jsdom
// An account an administrator has put in pending_password_change cannot sign
// in through SSO either: the OIDC callback bounces to
// /login?sso_error=password_change_required. The sign-in page has to say what
// is required AND how an SSO account does it — there is no password to type in
// the form above, so the only ways are the emailed reset ("Forgot your
// password?") and the administrator. The generic "SSO sign-in didn't complete.
// Please try again" sends them round the loop that just refused them.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/views/CloudLanding", () => ({ default: () => null, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));

import App from "@/App";
import { cancelScheduledRefresh } from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

beforeEach(() => {
  localStorage.clear();
  useServerInfoStore.setState({ info: { mode: "cloud", auth_required: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [{ name: "keycloak", display: "Acme SSO" }] });
    // No session: the callback bounced the browser back here without one.
    if (path === "/api/auth/me") return json({ error: "authentication required" }, 401);
    if (path === "/api/auth/refresh") return json({ error: "authentication required" }, 401);
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

describe("an SSO sign-in refused because a password change is required", () => {
  it("names the requirement and the two ways an SSO account can satisfy it", async () => {
    window.history.replaceState({}, "", "/login?sso_error=password_change_required&sso_provider=Acme%20SSO");
    render(<App />);
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    const notice = await screen.findByText(/password change/i, {}, { timeout: 8000 });
    console.log("[sso_error] notice:", JSON.stringify(notice.textContent));
    expect(notice.textContent).toContain("Your administrator requires a password change before your next sign-in");
    // The provider is named, and so is the fact that this form holds no
    // password for such an account.
    expect(notice.textContent).toContain("Acme SSO");
    expect(notice.textContent).toContain("Forgot your password?");
    expect(notice.textContent).toContain("the link is emailed to you");
    expect(notice.textContent).toContain("ask your administrator");
    // Not the catch-all, which says to try the thing that just failed.
    expect(notice.textContent).not.toContain("Please try again, or use your email and password");
    // And the way out it names is a control on this page.
    expect(screen.getByRole("button", { name: "Forgot your password?" })).toBeTruthy();
  }, 20000);

  it("keeps the catch-all for a code it does not know", async () => {
    window.history.replaceState({}, "", "/login?sso_error=exchange_failed&sso_provider=Acme%20SSO");
    render(<App />);
    const notice = await screen.findByText(/didn't complete/, {}, { timeout: 8000 });
    console.log("[sso_error/other] notice:", JSON.stringify(notice.textContent));
    expect(notice.textContent).toContain("Acme SSO sign-in didn't complete");
  }, 20000);
});
