// @vitest-environment jsdom
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useRoute } from "wouter";

// Exercise the real App routes, AuthProvider and Login together. Only the
// destination view/chrome and the HTTP boundary are replaced.
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

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import App from "@/App";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

// Mirror main.tsx: the real app always mounts App inside a QueryClientProvider.
// AuthProvider now reads the query client (to invalidate run-scoped caches on
// scope switch), so the test harness must provide one too.
function renderApp() {
  const client = new QueryClient();
  return render(
    <QueryClientProvider client={client}>
      <App />
    </QueryClientProvider>,
  );
}

const run = "/runs/review-123?tab=events#node-converge";
const identity = { user: { id: "u", email: "reviewer@example.test", status: "active", is_super_admin: true }, orgs: [], active_role: "owner" };
let authenticated: boolean;
let rejectLogin: boolean;
let restricted: boolean;

beforeEach(() => {
  authenticated = false;
  rejectLogin = false;
  restricted = false;
  window.history.replaceState({}, "", run);
  useServerInfoStore.setState({ info: { mode: "cloud", marketplace_enabled: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (path === "/api/auth/login") {
      if (rejectLogin) return json({ error: "Invalid credentials" }, 401);
      authenticated = true;
    }
    if (["/api/auth/me", "/api/auth/refresh", "/api/auth/login"].includes(path)) {
      return authenticated ? json(restricted ? { ...identity, user: { ...identity.user, is_super_admin: false }, active_role: "" } : identity) : json({ error: "Sign in required" }, 401);
    }
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

async function submitLogin() {
  fireEvent.change(await screen.findByPlaceholderText("Email"), { target: { value: "reviewer@example.test" } });
  fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "test-password" } });
  fireEvent.click(screen.getByRole("button", { name: /^Sign in$/ }));
}

describe("run → sign-in → requested run", () => {
  it("shows sign-in and returns to the full run URL after password login", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "Sign in to iterion" });
    expect(window.location.pathname).toBe("/login");
    expect(new URLSearchParams(window.location.search).get("next")).toBe(run);
    await submitLogin();
    await screen.findByRole("heading", { name: "Requested run review-123" });
    expect(window.location.pathname + window.location.search + window.location.hash).toBe(run);
    expect(screen.queryByText("Signed-in home")).toBeNull();
  });

  it("retains the destination when a password attempt fails", async () => {
    rejectLogin = true;
    renderApp();
    await submitLogin();
    await screen.findByText("Invalid credentials");
    expect(new URLSearchParams(window.location.search).get("next")).toBe(run);
    rejectLogin = false;
    await submitLogin();
    await screen.findByRole("heading", { name: "Requested run review-123" });
  });

  it("honors a login return link when the session already exists", async () => {
    authenticated = true;
    window.history.replaceState({}, "", `/login?${new URLSearchParams({ next: run })}`);
    renderApp();
    await screen.findByRole("heading", { name: "Requested run review-123" });
    expect(window.location.pathname + window.location.search + window.location.hash).toBe(run);
  });

  it("retains the restricted-account gate after signing in", async () => {
    restricted = true;
    renderApp();
    await submitLogin();
    await screen.findByRole("heading", { name: "Restricted account" });
    expect(screen.queryByText("Requested run review-123")).toBeNull();
  });

  it("keeps the public home page public", async () => {
    window.history.replaceState({}, "", "/");
    renderApp();
    await screen.findByRole("heading", { name: "Public cloud home" });
    await waitFor(() => expect(window.location.pathname).toBe("/"));
  });
});
