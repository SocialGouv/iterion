// @vitest-environment jsdom
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { useParams } from "wouter";

vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));

// Stands in for the real editor with the only two behaviours under test: it
// reads the share id from the route, and when there is none it renders its own
// diagnostic rather than nothing. Both are what ConfigShare/index.tsx does
// (useParams at :198, the "Invalid link" banner at :212).
vi.mock("@/views/ConfigShare", () => ({
  default: function ConfigShareStub() {
    const params = useParams<{ id?: string }>();
    return params?.id ? <h1>Editing share {params.id}</h1> : <h1>Invalid link</h1>;
  },
}));

import App from "@/App";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const identity = { user: { id: "u", email: "op@example.test", status: "active", is_super_admin: true }, orgs: [], active_role: "owner" };

beforeEach(() => {
  useServerInfoStore.setState({ info: { mode: "cloud", marketplace_enabled: true } as ServerInfo, loading: false, error: null });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (b: unknown, s = 200) => new Response(JSON.stringify(b), { status: s });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud", marketplace_enabled: true });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (["/api/auth/me", "/api/auth/refresh"].includes(path)) return json(identity);
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

// A config-share link is handed to someone who is NOT an operator, and it is
// opened just as often by one who is — in the same browser, with a session.
// That authenticated arm is the one that keeps breaking.
describe("the config-share arm, signed in", () => {
  // Rendered bare, the editor got no route params and told a visitor with a
  // perfectly good link that the id was missing.
  it("gives the editor its share id", async () => {
    window.history.replaceState({}, "", "/config/share-123#tok");
    render(<App />);
    expect(await screen.findByRole("heading", { name: "Editing share share-123" })).toBeTruthy();
  });

  // And the fix for that must not eat the other case: a lone <Switch> whose
  // single route misses renders NOTHING — /config/ became an empty page with
  // no text and no way out, replacing a diagnostic that was correct.
  it("still says what is wrong when the link carries no id", async () => {
    window.history.replaceState({}, "", "/config/");
    render(<App />);
    expect(await screen.findByRole("heading", { name: "Invalid link" })).toBeTruthy();
  });

  it("and when the link is shaped wrong", async () => {
    window.history.replaceState({}, "", "/config/a/b");
    render(<App />);
    expect(await screen.findByRole("heading", { name: "Invalid link" })).toBeTruthy();
  });
});
