// @vitest-environment jsdom
// A workspace pane's credentials live in the operating system's keychain, held
// by the desktop. When that keychain is locked the desktop can authenticate
// nothing and answers the pane 503 keychain_locked — which is neither "the
// server is down" nor "the server could not load your account": the way out is
// on this computer, and the pane has to say so.
//
// It also has to stop asking. Every keychain read raises an OS unlock prompt,
// so the identity-unavailable screen's 3-second auto-retry would be a password
// dialog every three seconds. Nothing is re-read until the user asks, and the
// ask carries X-Iterion-Keychain-Retry so the desktop reads NOW rather than
// waiting out its own back-off.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Router } from "wouter";

vi.hoisted(() => {
  (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__ = "/x/conn-1";
});

vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => null, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: () => <h1>Requested run</h1> }));

import App from "@/App";
import { cancelScheduledRefresh } from "@/api/client";
import { IDENTITY_RETRY_INTERVAL_MS } from "@/auth/AuthContext";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const account = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
};

let keychainLocked: boolean;
// Every /auth/me the desktop served, with the retry header the pane sent — a
// keychain read each, which is an unlock prompt each.
let reads: string[];

beforeEach(() => {
  localStorage.clear();
  keychainLocked = true;
  reads = [];
  window.history.replaceState({}, "", "/x/conn-1/studio/runs/review-123");
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
  vi.spyOn(window, "postMessage").mockImplementation(() => undefined);
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/x/conn-1/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/x/conn-1/api/auth/me") {
      const headers = (init?.headers ?? {}) as Record<string, string>;
      reads.push(`retry-header=${headers["X-Iterion-Keychain-Retry"] ?? "none"}`);
      if (keychainLocked) {
        return json({ error: "keychain locked: unlock your system keychain to use this connection", error_code: "keychain_locked" }, 503);
      }
      return json({ ...account, session_lineage: "L0" });
    }
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

describe("a pane whose desktop could not read a locked keychain", () => {
  it("names the keychain, asks nothing more on its own, and reads again only when told to", async () => {
    render(<Router base="/x/conn-1"><App /></Router>);
    await screen.findByText("Your system keychain is locked", {}, { timeout: 6000 });
    // Not the screen for a server that cannot answer: that one blames the
    // server and retries by itself.
    expect(screen.queryByText("Your identity is unavailable")).toBeNull();
    expect(screen.queryByText(/could not load your account/)).toBeNull();
    expect(screen.queryByText("Session expired")).toBeNull();

    // One read, and it stays one: the 503 is not a hiccup to retry (fetchIdentity's
    // own three attempts) and no screen re-reads on a cadence.
    await new Promise((r) => setTimeout(r, IDENTITY_RETRY_INTERVAL_MS + 1500));
    console.log("[keychain] reads while the screen stood:", JSON.stringify(reads));
    expect(reads).toEqual(["retry-header=none"]);

    // The user unlocked it and says so. That read carries the header, and the
    // desktop treats it as a user action.
    keychainLocked = false;
    screen.getByRole("button", { name: "I unlocked it — retry" }).click();
    await screen.findByRole("heading", { name: "Requested run" }, { timeout: 8000 });
    console.log("[keychain] reads after the retry:", JSON.stringify(reads));
    expect(reads).toEqual(["retry-header=none", "retry-header=1"]);
    expect(screen.queryByText("Your system keychain is locked")).toBeNull();
  }, 20000);

  it("stays on the keychain screen when the retry finds it still locked", async () => {
    render(<Router base="/x/conn-1"><App /></Router>);
    await screen.findByText("Your system keychain is locked", {}, { timeout: 6000 });
    screen.getByRole("button", { name: "I unlocked it — retry" }).click();
    await new Promise((r) => setTimeout(r, 500));
    console.log("[keychain/still-locked] reads:", JSON.stringify(reads));
    expect(reads).toEqual(["retry-header=none", "retry-header=1"]);
    expect(await screen.findByText("Your system keychain is locked")).toBeTruthy();
  }, 20000);
});
