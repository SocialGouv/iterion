// @vitest-environment jsdom
//
// T9 — rendering/gating + a11y for the admin console screens added by the
// platform-settings, spend and org-teams work. Each screen gates on two
// things before any network: the caller's is_super_admin and the deployment
// mode. Those gate states are pure (no fetch), so they're the honest place to
// assert both the authorization behaviour and the accessibility of what a
// non-operator / local user actually sees. The happy-path rendering + full
// interaction remains covered by the per-helper unit tests and Playwright.

import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { expectNoViolations, setupMatchMedia } from "@/__tests__/a11y/axeHelpers";

setupMatchMedia();

// Both gate inputs are mocked; each test sets them before rendering.
let mockUser: { is_super_admin?: boolean } | null = null;
let mockInfo: { mode?: string } | null = null;

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: mockUser }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) => sel({ info: mockInfo }),
}));

// AdminNav uses wouter's useLocation; stub it so the nav renders without a Router.
vi.mock("wouter", () => ({
  useLocation: () => ["/admin/settings/usage-caps", vi.fn()],
}));

import UsageCapsPage from "../settings/UsageCapsPage";
import BotRolesPage from "../settings/BotRolesPage";
import SandboxPage from "../settings/SandboxPage";
import BotVarsPage from "../settings/BotVarsPage";
import PlatformCredentialsPage from "../settings/PlatformCredentialsPage";
import CredentialSpendPage from "../spend/CredentialSpendPage";

const SCREENS: Record<string, () => ReactNode> = {
  "Usage caps": () => <UsageCapsPage />,
  "Bot roles": () => <BotRolesPage />,
  Sandbox: () => <SandboxPage />,
  "Bot vars": () => <BotVarsPage />,
  "Platform credentials": () => <PlatformCredentialsPage />,
  "Credential spend": () => <CredentialSpendPage />,
};

function mount(node: ReactNode): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <main>{node}</main>
    </QueryClientProvider>,
  ).container;
}

beforeEach(() => {
  // No screen should reach the network in these gate states, but stub fetch so
  // an accidental request can't hit the real transport.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } })),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  mockUser = null;
  mockInfo = null;
});

describe("admin screens · gating", () => {
  for (const [name, render_] of Object.entries(SCREENS)) {
    it(`${name} — shows "Super-admin only" for a non-super-admin (cloud)`, () => {
      mockUser = { is_super_admin: false };
      mockInfo = { mode: "cloud" };
      mount(render_());
      expect(screen.getByText(/super-admin only/i)).toBeTruthy();
    });

    it(`${name} — shows the cloud-only notice in local mode`, () => {
      mockUser = { is_super_admin: true }; // local synthesises a super-admin
      mockInfo = { mode: "local" };
      mount(render_());
      // CloudOnlyNotice renders copy naming the feature as cloud-only.
      expect(screen.getByText(/cloud/i)).toBeTruthy();
    });
  }
});

describe("admin screens · a11y of the gate states", () => {
  it("non-super-admin notice has no axe violations", async () => {
    mockUser = { is_super_admin: false };
    mockInfo = { mode: "cloud" };
    await expectNoViolations(mount(<UsageCapsPage />), "UsageCapsPage/403");
  });

  it("cloud-only notice has no axe violations", async () => {
    mockUser = { is_super_admin: true };
    mockInfo = { mode: "local" };
    await expectNoViolations(mount(<CredentialSpendPage />), "CredentialSpendPage/cloud-only");
  });
});
