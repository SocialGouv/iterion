// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { ForgeOAuthApp } from "@/api/forgeConnections";

let mockUser: { is_super_admin?: boolean } | null = null;
let mockInfo: { mode?: string } | null = null;

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: mockUser }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) => sel({ info: mockInfo }),
}));

const listPlatformForgeOAuthApps = vi.fn<() => Promise<ForgeOAuthApp[]>>();
const registerPlatformForgeOAuthApp = vi.fn();
const deletePlatformForgeOAuthApp = vi.fn();
vi.mock("@/api/forgeConnections", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/forgeConnections")>();
  return {
    ...actual,
    listPlatformForgeOAuthApps: () => listPlatformForgeOAuthApps(),
    registerPlatformForgeOAuthApp: (input: unknown) => registerPlatformForgeOAuthApp(input),
    deletePlatformForgeOAuthApp: (id: string) => deletePlatformForgeOAuthApp(id),
  };
});

import PlatformForgeAppsPage from "./PlatformForgeAppsPage";
import { ADMIN_BASE } from "./adminRoutes";

const picApp = {
  id: "app-1",
  tenant_id: "platform",
  provider: "gitlab",
  forge_base_url: "https://pic.example",
  client_id: "cid-123",
  webhook_base_url: "https://iterion.fabrique.example",
  platform: true,
  created_at: "2026-10-01T00:00:00Z",
} as unknown as ForgeOAuthApp;

function mountPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const loc = memoryLocation({ path: `/studio${ADMIN_BASE}/forge-apps` });
  render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base="/studio">
        <Route path={ADMIN_BASE} nest>
          <main>
            <PlatformForgeAppsPage />
          </main>
        </Route>
      </Router>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  listPlatformForgeOAuthApps.mockResolvedValue([picApp]);
  registerPlatformForgeOAuthApp.mockResolvedValue({ ...picApp, id: "app-2" });
  deletePlatformForgeOAuthApp.mockResolvedValue(undefined);
  mockUser = { is_super_admin: true };
  mockInfo = { mode: "cloud" };
});
afterEach(() => {
  cleanup();
  mockUser = null;
  mockInfo = null;
});

describe("PlatformForgeAppsPage", () => {
  it("lists the platform apps with their instance and inherited hook base", async () => {
    mountPage();
    expect(await screen.findByText("cid-123")).toBeTruthy();
    screen.getByText("https://pic.example");
    screen.getByText("https://iterion.fabrique.example");
    expect(screen.getByRole("navigation", { name: "Admin console" })).toBeTruthy();
  });

  it("registers a pasted application with its hook base", async () => {
    mountPage();
    await screen.findByText("cid-123");
    fireEvent.change(screen.getByLabelText("Forge base URL"), {
      target: { value: "https://pic.example" },
    });
    fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "new-cid" } });
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "new-secret" } });
    fireEvent.change(screen.getByLabelText("Webhook base URL"), {
      target: { value: "https://iterion.fabrique.example" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() => expect(registerPlatformForgeOAuthApp).toHaveBeenCalledTimes(1));
    expect(registerPlatformForgeOAuthApp).toHaveBeenCalledWith({
      provider: "gitlab",
      forge_base_url: "https://pic.example",
      client_id: "new-cid",
      client_secret: "new-secret",
      webhook_base_url: "https://iterion.fabrique.example",
    });
    // The secret never lingers in the form after the registration.
    expect((screen.getByLabelText("Client secret") as HTMLInputElement).value).toBe("");
  });

  it("refuses to register without both credentials", async () => {
    mountPage();
    await screen.findByText("cid-123");
    fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "only-id" } });
    expect((screen.getByRole("button", { name: "Register" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  it("deletes after confirmation", async () => {
    mountPage();
    await screen.findByText("cid-123");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete", hidden: false }));
    await waitFor(() => expect(deletePlatformForgeOAuthApp).toHaveBeenCalledWith("app-1"));
  });

  it("shows the super-admin notice to anyone else", () => {
    mockUser = { is_super_admin: false };
    mountPage();
    screen.getByText("Super-admin only.");
    expect(listPlatformForgeOAuthApps).not.toHaveBeenCalled();
  });
});
