// @vitest-environment jsdom
import type { ComponentType } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type {
  BotRolesSettingsView,
  BotVarsSettingsView,
  PlatformCredentialsSettingsView,
  SandboxSettingsView,
  UsageCapsView,
} from "@/api/adminSettings";
import type { CredentialUsageListView } from "@/api/adminCredUsage";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

// The two facts every console screen gates on. The guard around the group
// (RequireSuperAdmin, tested in auth/) means a real mount only ever happens in
// the allowed tier; these screens keep their own gate as defense-in-depth, so
// they are mounted here WITHOUT that guard — the only altitude at which the
// per-page gate is observable.
let mockUser: { is_super_admin?: boolean } | null = null;
let mockInfo: { mode?: string } | null = null;

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: mockUser }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) => sel({ info: mockInfo }),
}));

const getUsageCaps = vi.fn<() => Promise<UsageCapsView>>();
const getBotRoles = vi.fn<() => Promise<BotRolesSettingsView>>();
const getSandboxSettings = vi.fn<() => Promise<SandboxSettingsView>>();
const getBotVars = vi.fn<() => Promise<BotVarsSettingsView>>();
const getPlatformCredentialsSettings = vi.fn<() => Promise<PlatformCredentialsSettingsView>>();
const getAdminCredentialUsage = vi.fn<() => Promise<CredentialUsageListView>>();

vi.mock("@/api/adminSettings", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/adminSettings")>();
  return {
    ...actual,
    getUsageCaps: () => getUsageCaps(),
    getBotRoles: () => getBotRoles(),
    getSandboxSettings: () => getSandboxSettings(),
    getBotVars: () => getBotVars(),
    getPlatformCredentialsSettings: () => getPlatformCredentialsSettings(),
  };
});
vi.mock("@/api/adminCredUsage", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/adminCredUsage")>();
  return { ...actual, getAdminCredentialUsage: () => getAdminCredentialUsage() };
});

import UsageCapsPage from "./settings/UsageCapsPage";
import BotRolesPage from "./settings/BotRolesPage";
import SandboxPage from "./settings/SandboxPage";
import BotVarsPage from "./settings/BotVarsPage";
import PlatformCredentialsPage from "./settings/PlatformCredentialsPage";
import CredentialSpendPage from "./spend/CredentialSpendPage";
import { ADMIN_BASE } from "./adminRoutes";

setupMatchMedia();

// Each screen, the path it is mounted at, and one labelled field of its own
// form — the proof that the screen itself rendered and not just the chrome.
interface Screen {
  name: string;
  path: string;
  Page: ComponentType;
  /** Accessible name of a form control only this screen renders. */
  field: string | RegExp;
}

const SCREENS: Screen[] = [
  { name: "UsageCapsPage", path: "/settings/usage-caps", Page: UsageCapsPage, field: "5-hour window (%)" },
  // FieldLabel appends its `?` help affordance inside the <label>, so the
  // field's label text is "Reviewer?" and not the field name alone.
  { name: "BotRolesPage", path: "/settings/bot-roles", Page: BotRolesPage, field: "Reviewer?" },
  { name: "SandboxPage", path: "/settings/sandbox", Page: SandboxPage, field: "Default image" },
  { name: "BotVarsPage", path: "/settings/bot-vars", Page: BotVarsPage, field: "Variable name" },
  {
    name: "PlatformCredentialsPage",
    path: "/settings/platform-credentials",
    Page: PlatformCredentialsPage,
    field: "Allowed teams",
  },
  { name: "CredentialSpendPage", path: "/spend", Page: CredentialSpendPage, field: "Tier" },
];

function seedViews(): void {
  getUsageCaps.mockResolvedValue({
    effective: { five_hour_pct: 90, five_hour_mode: "block", week_pct: 90, week_mode: "block" },
    env: { five_hour_pct: 90, week_pct: 90 },
    propagation_bound_seconds: 60,
    source: "env",
  });
  getBotRoles.mockResolvedValue({
    effective: {
      reviewer: "revi",
      revi_converse: "revi-converse",
      brancher: "brancher",
      implementer: "implementer",
    },
    origin: "default",
  });
  getSandboxSettings.mockResolvedValue({
    effective_default_image: "ghcr.io/socialgouv/iterion-sandbox@sha256:" + "0".repeat(64),
    origin: "default",
  });
  getBotVars.mockResolvedValue({
    origin: "db",
    propagation_bound_seconds: 60,
    stored: { updated_at: "2026-10-04T00:00:00Z", vars: { ITERION_MAX_PARALLEL: "4" } },
  });
  getPlatformCredentialsSettings.mockResolvedValue({
    enforced: false,
    facade_default_effective: "anthropic",
    keys_first_effective: false,
    origin: "default",
    routing_effective_scope: "default",
  });
  // One credential row, not an empty list: the spend screen's table and its
  // per-row "Clear readings" action (the cap-reset escape hatch) only render
  // when there is a row, and an empty table exercises neither.
  getAdminCredentialUsage.mockResolvedValue({
    credentials: [
      {
        fingerprint: "e4ecd228",
        provider: "anthropic",
        tier: "platform",
        nature: "metered",
        month: "2026-10",
        cost_usd: 1.25,
        runs: 3,
        input_tokens: 1000,
        output_tokens: 250,
        aggregate_tokens: 1250,
      },
    ],
    estimated_usd: 0,
    metered_usd: 1.25,
    month: "2026-10",
    scope: { tier: "platform" },
  });
}

// The screen in its own place: inside <Router base="/studio"> and the console's
// nested route, so the AdminNav it renders resolves its links the way it does
// in the app.
function mountScreen(path: string, Page: ComponentType): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const loc = memoryLocation({ path: `/studio${ADMIN_BASE}${path}` });
  return render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base="/studio">
        <Route path={ADMIN_BASE} nest>
          <main>
            <Page />
          </main>
        </Route>
      </Router>
    </QueryClientProvider>,
  ).container;
}

beforeEach(() => {
  for (const fn of [
    getUsageCaps,
    getBotRoles,
    getSandboxSettings,
    getBotVars,
    getPlatformCredentialsSettings,
    getAdminCredentialUsage,
  ]) {
    fn.mockReset();
  }
  seedViews();
  mockUser = { is_super_admin: true };
  mockInfo = { mode: "cloud" };
});
afterEach(() => {
  cleanup();
  mockUser = null;
  mockInfo = null;
});

describe("admin console screens / per-page gate", () => {
  it.each(SCREENS)("$name renders for a super-admin in cloud mode", async ({ path, Page, field }) => {
    mountScreen(path, Page);
    expect(await screen.findByLabelText(field)).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Admin console" })).toBeTruthy();
    expect(screen.queryByText("Super-admin only.")).toBeNull();
  });

  it.each(SCREENS)("$name shows the super-admin notice, never the screen, for a non-super-admin", async ({ path, Page, field }) => {
    mockUser = { is_super_admin: false };
    mountScreen(path, Page);
    expect(await screen.findByText("Super-admin only.")).toBeTruthy();
    expect(screen.queryByLabelText(field)).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Admin console" })).toBeNull();
  });

  it.each(SCREENS)("$name degrades to the cloud-only notice in local mode", async ({ path, Page, field }) => {
    mockInfo = { mode: "local" };
    mountScreen(path, Page);
    expect(await screen.findByText(/cloud-mode feature/)).toBeTruthy();
    expect(screen.queryByLabelText(field)).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Admin console" })).toBeNull();
  });

  // T5's affordance lives in a row of the spend table, so it only exists once
  // the view carries a credential.
  it("CredentialSpendPage offers the per-credential cap-reset action on each row", async () => {
    mountScreen("/spend", CredentialSpendPage);
    expect(await screen.findByRole("table", { name: "Per-credential spend" })).toBeTruthy();
    expect(screen.getByText("e4ecd228")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Clear readings" })).toHaveLength(1);
  });
});

describe("a11y / admin console screens", () => {
  it.each(SCREENS)("$name — loaded form, nav and header wiring", async ({ name, path, Page, field }) => {
    const container = mountScreen(path, Page);
    await screen.findByLabelText(field);
    await expectNoViolationsStrict(container, name);
  });

  it.each(SCREENS)("$name — super-admin gate short-circuit", async ({ name, path, Page }) => {
    mockUser = { is_super_admin: false };
    const container = mountScreen(path, Page);
    await screen.findByText("Super-admin only.");
    await expectNoViolationsStrict(container, `${name} (gated)`);
  });

  it.each(SCREENS)("$name — local-mode notice", async ({ name, path, Page }) => {
    mockInfo = { mode: "local" };
    const container = mountScreen(path, Page);
    await screen.findByText(/cloud-mode feature/);
    await expectNoViolationsStrict(container, `${name} (local)`);
  });
});
