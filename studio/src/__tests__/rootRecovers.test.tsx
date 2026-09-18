// @vitest-environment jsdom
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";

// Only the destination views and the HTTP boundary are replaced; the real
// App, AuthProvider and stores run.
vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: ({ signedIn }: { signedIn?: boolean }) => <h1>Public cloud home{signedIn ? " (signed in)" : ""}</h1>, PublicTopBar: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));

import App from "@/App";
import { useServerInfoStore } from "@/store/serverInfo";

const identity = { user: { id: "u", email: "op@example.test", status: "active", is_super_admin: true }, orgs: [], active_role: "owner" };

// backendUp flips mid-test: the document boots while the server is down, then
// the server comes back. That is the ordinary rolling-deploy window, and it is
// the only window in which the defect is reachable.
let backendUp: boolean;

beforeEach(() => {
  backendUp = false;
  window.history.replaceState({}, "", "/");
  // The store starts EMPTY, exactly as it does in a document whose one-shot
  // boot probe lost. Not seeded — seeding it is what hides this.
  useServerInfoStore.setState({ info: null, loading: false, error: null });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (!backendUp) return json({ error: "backend down" }, 503);
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud", marketplace_enabled: true });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (["/api/auth/me", "/api/auth/refresh"].includes(path)) return json(identity);
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("the root recovers when the backend does", () => {
  // "/" cannot render until it knows the server mode — the product home and
  // the studio live at the same address and only the mode tells them apart.
  // The probe that answers it used to run exactly once, at module load, with
  // no retry: a backend down at first paint pinned the root on a boot spinner
  // for the life of the document, with nothing left to resolve it. Every other
  // address recovered; only the default entry URL did not.
  it("leaves the boot spinner once the server answers, instead of waiting forever", async () => {
    render(<App />);

    // Down: the gate shows its reconnect screen, and the mode is unknown.
    await screen.findByText("Can't reach the iterion server");
    expect(useServerInfoStore.getState().info).toBeNull();

    // Up: the auth probe re-runs on its own cadence and MUST also be what
    // re-fills the mode — nothing else fetches it a second time.
    backendUp = true;

    await waitFor(
      () => expect(screen.getByRole("heading", { name: "Public cloud home (signed in)" })).toBeTruthy(),
      { timeout: 8000 },
    );
    expect(window.location.pathname).toBe("/");
    expect(useServerInfoStore.getState().info?.mode).toBe("cloud");
  }, 15000);
});
