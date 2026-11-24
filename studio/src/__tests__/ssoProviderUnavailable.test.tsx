// @vitest-environment jsdom
// A per-org SSO whose provider row the server could not READ bounces to
// /login?sso_error=provider_unavailable (pkg/server oidc.ErrProviderUnavailable).
// The page must not reuse the unknown_provider text: "that provider is no
// longer available, use your email and password" sends an SSO-only account —
// which has no password — to a form that cannot sign it in, for an outage that
// will be over in a minute. It says temporary, and it says try again.
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

describe("an SSO sign-in refused because the provider could not be read", () => {
  it("says the outage is temporary and to try again, never that the provider is gone", async () => {
    window.history.replaceState({}, "", "/login?sso_error=provider_unavailable&sso_provider=Acme%20SSO");
    render(<App />);
    await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
    const notice = await screen.findByText(/temporarily unavailable/i, {}, { timeout: 8000 });
    expect(notice.textContent).toContain("Acme SSO");
    expect(notice.textContent).toContain("try again in a moment");
    // Not the unknown_provider text: the provider is not gone.
    expect(notice.textContent).not.toContain("no longer available");
    // Not the catch-all either, which names no cause and no next step.
    expect(notice.textContent).not.toContain("didn't complete");
  }, 20000);

  it("keeps the unknown_provider text for a provider that is really gone", async () => {
    window.history.replaceState({}, "", "/login?sso_error=unknown_provider&sso_provider=Acme%20SSO");
    render(<App />);
    const notice = await screen.findByText(/no longer available/i, {}, { timeout: 8000 });
    expect(notice.textContent).toContain("Try signing in with your email and password");
    expect(notice.textContent).not.toContain("temporarily unavailable");
  }, 20000);
});
