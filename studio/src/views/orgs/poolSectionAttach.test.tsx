// @vitest-environment jsdom
// The key-pool attachment editor (ADR-164) must offer the ATTACH form on a
// level with nothing attached — the server answers attached=false there —
// and hide only when the server lacks the feature (the client's null).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { getTeamPoolAttachment, type PoolAttachment } from "@/api/pools";

const state = vi.hoisted(() => ({
  attachment: null as PoolAttachment | null,
}));

vi.mock("@/api/pools", async () => {
  const actual = await vi.importActual<typeof import("@/api/pools")>("@/api/pools");
  return {
    ...actual,
    getTeamPoolAttachment: vi.fn(async () => state.attachment),
    getOrgPoolAttachment: vi.fn(async () => state.attachment),
    listPoolEntries: vi.fn(async () => []),
  };
});

import PoolSection from "./PoolSection";

afterEach(cleanup);

beforeEach(() => {
  state.attachment = null;
});

function renderSection(scope: "org" | "team") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <PoolSection scope={scope} id="x1" canManage />
    </QueryClientProvider>,
  );
}

describe("PoolSection attach", () => {
  it.each(["org", "team"] as const)("offers the attach form on an unattached %s level", async (scope) => {
    state.attachment = {
      scope_id: `${scope}:x1`,
      attached: false,
      pool_id: "",
      enabled: false,
      strategy: {},
      limits: {},
    };
    renderSection(scope);
    expect(await screen.findByLabelText("Pool id to attach")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Attach" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Detach (inherit)" })).toBeNull();
  });

  it("shows an attached level with its pool and the detach action", async () => {
    state.attachment = {
      scope_id: "team:x1",
      attached: true,
      pool_id: "pool-forfaits",
      enabled: true,
      strategy: { mode: "priority" },
      limits: {},
      updated_at: new Date().toISOString(),
    };
    renderSection("team");
    expect(await screen.findByText("pool-forfaits")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Detach (inherit)" })).toBeTruthy();
    expect(screen.queryByLabelText("Pool id to attach")).toBeNull();
  });

  it("stays hidden on a server without the feature", async () => {
    state.attachment = null;
    const { container } = renderSection("team");
    // Settled: the read answered and the spinner is gone.
    await vi.waitFor(() => {
      expect(getTeamPoolAttachment).toHaveBeenCalled();
      expect(container.querySelector(".animate-spin")).toBeNull();
    });
    expect(screen.queryByText("Key pool")).toBeNull();
    expect(screen.queryByLabelText("Pool id to attach")).toBeNull();
  });
});
