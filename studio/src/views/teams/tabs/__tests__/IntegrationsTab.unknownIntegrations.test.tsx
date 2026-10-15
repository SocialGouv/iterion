// @vitest-environment jsdom
// The chain a posture is decided in, driven through the REAL tab: the
// Integrations tab reads the team's integrations for what it displays, a
// connection card holds the slice for its own list, and the Enable panel reads
// that same cache key ITSELF for what it decides. A read in flight, a read in
// error, and the snapshot query-core keeps beside such an error are all
// "unknown": a panel reading one of them as "this repo is new" would offer —
// and pin — a posture the repo already has, as a partial launch_vars the
// server refuses.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { ForgeIntegration } from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeConnections: vi.fn(async () => [
      {
        id: "c1",
        provider: "gitlab",
        kind: "pat",
        status: "active",
        account_login: "iterion-bot",
        created_at: "2026-10-01T00:00:00Z",
      },
    ]),
    listForgeIntegrations: vi.fn(),
    listForgeOAuthApps: vi.fn(async () => []),
    listForgeRepos: vi.fn(async () => [{ full_name: "org/api", can_admin: true }]),
    previewForgeEnable: vi.fn(async () => ({
      events_normalized: ["merge_request"],
      forge_native_events: ["merge_request"],
      scopes: {},
      secrets: [],
      commands: [],
      identity: { handle: "iterion-bot", provider: "gitlab", base_url: "https://pic.sg" },
      gates_merges: true,
      hook_base_url: "https://iterion.fabrique.social.gouv.fr",
      conflicts: [],
    })),
    enableForgeRepoBots: vi.fn(async () => ({
      integration_id: "i1",
      webhook_id: "w1",
      hook_id: "h1",
      managed_secret_id: "s1",
      bot_ids: ["review-pr"],
      created: false,
    })),
    listForgeIntegrationHooks: vi.fn(async () => []),
  };
});

vi.mock("@/api/orgGovernance", async () => {
  const actual = await vi.importActual<typeof import("@/api/orgGovernance")>("@/api/orgGovernance");
  return { ...actual, listTeamProvisionApprovals: vi.fn(async () => []) };
});

vi.mock("@/api/schedules", async () => {
  const actual = await vi.importActual<typeof import("@/api/schedules")>("@/api/schedules");
  return { ...actual, listTeamSchedules: vi.fn(async () => []) };
});

vi.mock("@/api/bots", async () => {
  const actual = await vi.importActual<typeof import("@/api/bots")>("@/api/bots");
  return {
    ...actual,
    listBotsWithDiagnostics: vi.fn(async () => ({
      bots: [
        {
          name: "review-pr",
          display_name: "Revi",
          invocations: [{ kind: "forge", forge: { event: "merge_request" } }],
        },
      ],
      discoveryErrors: [],
    })),
  };
});

import { enableForgeRepoBots, listForgeIntegrations } from "@/api/forgeConnections";
import IntegrationsTab from "../IntegrationsTab";

const integrationsMock = vi.mocked(listForgeIntegrations);
const enableMock = vi.mocked(enableForgeRepoBots);

// The repo IS enabled on this connection, advisory-only, and pins a review
// tier: restating launch_vars from the panel would drop it (the server
// refuses that write), so the panel must never offer the posture here.
const repoIntegration = {
  id: "i1",
  connection_id: "c1",
  provider: "gitlab",
  repo_full_name: "org/api",
  bot_ids: ["review-pr"],
  events_normalized: ["merge_request"],
  webhook_id: "w1",
  hook_id: "h1",
  created_at: "2026-10-01T00:00:00Z",
  launch_vars: { gate_enabled: "false", review_tier: "audit" },
} as ForgeIntegration;

const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

const renderTab = (client: QueryClient = newClient()) =>
  render(
    <QueryClientProvider client={client}>
      <IntegrationsTab teamID="t1" canManage />
    </QueryClientProvider>,
  );

/** Open the card's Enable panel on the repo and the gating bot. */
async function openEnablePanel() {
  fireEvent.click(await screen.findByRole("button", { name: "+ Enable a repo" }));
  await screen.findByRole("option", { name: /org\/api/ });
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
  fireEvent.click(screen.getByRole("checkbox", { name: /review-pr/ }));
  // The preview for this selection has landed: what is missing from here on
  // is about the integrations and nothing else.
  await screen.findByText("https://iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/…");
}

beforeEach(() => vi.clearAllMocks());
afterEach(cleanup);

describe("Integrations tab → connection card → Enable panel: an unknown list stays unknown", () => {
  it("offers no posture when the tab's integrations read FAILED", async () => {
    integrationsMock.mockRejectedValue(new Error("API error 500: store down"));
    renderTab();
    await screen.findByText(/store down/);

    await openEnablePanel();

    // Nothing on this screen knows whether org/api is already enabled here.
    await screen.findByText(/this team.s enabled repos could not be read/);
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();

    // And the enable carries no posture: it keeps whatever the repo pins.
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });

  it("offers no posture, and holds Enable, while the tab's integrations read is in flight", async () => {
    integrationsMock.mockImplementation(() => new Promise(() => {}));
    renderTab();

    await openEnablePanel();

    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();
    // The selection gates merges and the posture cannot be decided yet: Enable
    // waits and says why, instead of sending the default posture.
    await screen.findByText(/reading this team.s enabled repos/);
    const enable = screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
    expect(enable.disabled).toBe(true);
    fireEvent.click(enable);
    await Promise.resolve();
    expect(enableMock).not.toHaveBeenCalled();
  });

  it("offers no posture off a cached list the tab could not refresh", async () => {
    // The operator was on this tab earlier, so a snapshot sits under the
    // shared key; it predates org/api's integration. The refresh fails.
    // query-core keeps that snapshot NEXT TO the error, which is why a
    // present list is not a current one — and the panel, which owns the read
    // it decides on, says so instead of offering a posture the repo pins.
    const client = newClient();
    client.setQueryData<ForgeIntegration[]>(["forge-integrations", "t1"], []);
    integrationsMock.mockRejectedValue(new Error("API error 500: store down"));
    renderTab(client);
    await screen.findByText(/store down/);

    await openEnablePanel();

    await screen.findByText(/this team.s enabled repos could not be read/);
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });

  it("reads the stored posture once the list arrives and names the repo page", async () => {
    integrationsMock.mockResolvedValue([repoIntegration]);
    renderTab();

    await openEnablePanel();

    await screen.findByText(/Merge gate: Advisory only/);
    expect(
      screen.getByRole("link", { name: "change it on the repo page" }).getAttribute("href"),
    ).toBe("/repos/c1%3A%3Aorg%2Fapi");
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
  });

  it("offers the posture for a repo a fresh list does not carry", async () => {
    integrationsMock.mockResolvedValue([]);
    renderTab();

    await openEnablePanel();

    // The chain still passes a real answer through: an empty list the tab HAS
    // read is a fact about the team, and this repo is new on this connection.
    const gate = await screen.findByRole("radio", { name: /Gate merges/ });
    expect((gate as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("radio", { name: /Advisory only/ }));
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[5]).toEqual({ gate_enabled: "false" });
  });
});
