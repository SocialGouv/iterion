// @vitest-environment jsdom
// The warning a sign-out the server did not confirm leaves promises only what
// the thing HOLDING that pending sign-out can keep.
//
// With localStorage, the mark is the browser's: every tab reads it, a reload
// finds it, and nothing here adopts that session again — "iterion signs no one
// back into it" is true. Where the browser refuses site data (Chrome's "block
// site data" throws on access, Firefox's dom.storage.enabled=false gives
// none), the mark lives in this document and dies with it: a reload or a new
// tab reads the cookies as any other session's. The warning has to say which
// of the two the user is in, because what they should DO differs — retry here
// until it is confirmed, or trust the browser to remember.
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

vi.mock("@/components/shared/AppShell", async () => {
  const { Suspense: S } = await import("react");
  const { useAuth } = await import("@/auth/AuthContext");
  return {
    default: function ShellStandIn({ children }: { children: ReactNode }) {
      const { signOut } = useAuth();
      return (
        <S fallback={null}>
          <button onClick={() => void signOut()}>Sign out</button>
          {children}
        </S>
      );
    },
  };
});
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => null }));
vi.mock("@/views/ConfigShare", () => ({ default: () => null }));
vi.mock("@/views/Marketplace", () => ({ default: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: () => <h1>Requested run</h1> }));

import App from "@/App";
import { cancelScheduledRefresh } from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const account = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};

const realStorage = Object.getOwnPropertyDescriptor(globalThis, "localStorage");

function refuseStorage() {
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    get() {
      throw new DOMException("The operation is insecure.", "SecurityError");
    },
  });
}

async function signOutUnconfirmed(): Promise<string> {
  render(<App />);
  await screen.findByRole("heading", { name: "Requested run" }, { timeout: 8000 });
  screen.getByRole("button", { name: "Sign out" }).click();
  const alert = await screen.findByRole("alert", {}, { timeout: 8000 });
  return alert.textContent ?? "";
}

beforeEach(() => {
  window.history.replaceState({}, "", "/studio/runs/review-123");
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (path === "/api/auth/me") return json({ ...account, session_lineage: "L0" });
    // The revocation failed: the session is still the server's, and the
    // cookies are still the browser's.
    if (path === "/api/auth/logout") return json({ error: "identity unavailable" }, 500);
    throw new Error(`unexpected ${path}`);
  }));
});

afterEach(() => {
  if (realStorage) Object.defineProperty(globalThis, "localStorage", realStorage);
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("the warning for a sign-out the server did not confirm", () => {
  it("promises that no one is signed back in where the mark is the browser's", async () => {
    localStorage.clear();
    const text = await signOutUnconfirmed();
    console.log("[browser-scope] banner:", text);
    expect(text).toContain("Sign-out not confirmed");
    expect(text).toContain("iterion signs no one back into it");
    expect(text).not.toContain("may sign you back in");
  }, 20000);

  it("says the mark dies with this tab where the browser refuses storage", async () => {
    refuseStorage();
    const text = await signOutUnconfirmed();
    console.log("[tab-scope] banner:", text);
    expect(text).toContain("Sign-out not confirmed");
    // The promise the browser cannot keep is not made...
    expect(text).not.toContain("iterion signs no one back into it");
    // ...and what a reload or a new tab would do is said instead.
    expect(text).toContain("will not keep that warning past this tab");
    expect(text).toContain("may sign you back in");
  }, 20000);
});
