// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotVarsSettingsView } from "@/api/adminSettings";

const getBotVars = vi.fn<() => Promise<BotVarsSettingsView>>();

vi.mock("@/api/adminSettings", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/adminSettings")>();
  return { ...actual, getBotVars: () => getBotVars() };
});
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: { is_super_admin: true } }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: { mode: string } | null }) => unknown) =>
    sel({ info: { mode: "cloud" } }),
}));
vi.mock("@/views/admin/AdminNav", () => ({ default: () => null }));

import BotVarsPage from "./BotVarsPage";

function view(over: Partial<BotVarsSettingsView> = {}): BotVarsSettingsView {
  return {
    origin: "db",
    propagation_bound_seconds: 60,
    stored: {
      updated_at: "2026-10-04T00:00:00Z",
      vars: { ITERION_MAX_PARALLEL: "4" },
    },
    ...over,
  };
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <BotVarsPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  getBotVars.mockReset();
});
afterEach(cleanup);

describe("BotVarsPage", () => {
  // The refused map is the one field that changes what the operator should
  // DO: a refused entry still shows up in the stored rows, so without the
  // banner it reads as applied while the server hands out nothing for it.
  it("shows each refused entry with its reason, apart from the live rows", async () => {
    getBotVars.mockResolvedValue(
      view({
        stored: {
          updated_at: "2026-10-04T00:00:00Z",
          vars: { ITERION_MAX_PARALLEL: "4", ITERION_MODEL: "bad value" },
        },
        refused: { ITERION_MODEL: 'value carries "~"' },
      }),
    );
    renderPage();
    const banner = await screen.findByText(/ITERION_MODEL is stored but refused/);
    expect(banner).toBeTruthy();
    expect(screen.getByText(/value carries/)).toBeTruthy();
    // The conforming override stays a plain row: no refusal banner for it.
    expect(screen.queryByText(/ITERION_MAX_PARALLEL is stored but refused/)).toBeNull();
  });

  it("renders no refusal banner when every stored entry conforms", async () => {
    getBotVars.mockResolvedValue(view());
    renderPage();
    // The override itself is an editable row (an input value, not text).
    expect(await screen.findByDisplayValue("ITERION_MAX_PARALLEL")).toBeTruthy();
    expect(screen.queryByText(/is stored but refused/)).toBeNull();
  });
});
