// @vitest-environment jsdom
// The repo's issue-label filter, driven through the REAL Integrations tab: the
// tab owns the one error banner, a connection card lists the enabled repos,
// and the Enable panel writes the filter. Two things are only true at this
// height — a refused save is ON SCREEN (the panel reports into the tab's
// banner, it renders no error itself), and an operator reads the active filter
// off the repo's row without opening the panel.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { apiErrorFrom } from "@/api/client";
import type { ForgeIntegration } from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeConnections: vi.fn(async () => [
      {
        id: "c1",
        provider: "github",
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
      events_normalized: ["issues"],
      forge_native_events: ["issues"],
      scopes: {},
      secrets: [],
      commands: [],
      identity: { handle: "iterion-bot", provider: "github", base_url: "https://github.com" },
      gates_merges: false,
      hook_base_url: "https://iterion.example.org",
      conflicts: [],
    })),
    enableForgeRepoBots: vi.fn(),
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
          name: "featurly",
          display_name: "Featurly",
          invocations: [{ kind: "forge", forge: { event: "issues" } }],
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

const integration = (over: Partial<ForgeIntegration> = {}) =>
  ({
    id: "i1",
    connection_id: "c1",
    provider: "github",
    repo_full_name: "org/api",
    bot_ids: ["featurly"],
    events_normalized: ["issues"],
    webhook_id: "w1",
    hook_id: "h1",
    created_at: "2026-10-01T00:00:00Z",
    ...over,
  }) as ForgeIntegration;

const renderTab = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <IntegrationsTab teamID="t1" canManage />
    </QueryClientProvider>,
  );

/** The row of the connection card's "Enabled repos" list for a repo. */
async function enabledRepoRow(repo: string) {
  const link = await screen.findByTitle(`Repository details — ${repo}`);
  const row = link.closest("li");
  if (!row) throw new Error(`no enabled-repo row around ${repo}`);
  return row;
}

beforeEach(() => vi.clearAllMocks());
afterEach(cleanup);

describe("Integrations tab — the enabled repo's label filter is read off its row", () => {
  it("names the labels that start the repo's bots", async () => {
    integrationsMock.mockResolvedValue([integration({ label_allowlist: ["securite", "bug"] })]);
    renderTab();

    const row = await enabledRepoRow("org/api");
    const summary = within(row).getByText(/starts only on issue labels:/);
    expect(summary.textContent).toBe(" · starts only on issue labels: securite, bug");
  });

  it("says nothing of a filter on a repo that narrows nothing", async () => {
    // The server omits an empty list: absent and empty are the same repo.
    integrationsMock.mockResolvedValue([integration()]);
    renderTab();

    const row = await enabledRepoRow("org/api");
    expect(within(row).queryByText(/starts only on issue labels:/)).toBeNull();
  });
});

describe("Integrations tab → Enable panel: a refused label-filter save is shown", () => {
  it("puts the server's refusal in the banner and keeps the edit in the panel", async () => {
    integrationsMock.mockResolvedValue([integration({ label_allowlist: ["securite"] })]);
    // Clearing a filter widens dispatch. On an org that requires provisioning
    // approval the request is parked, and the park itself is refused (409)
    // while another request for the repo is pending — in the store's words.
    const refusal = "forge: a provisioning request for org/api is already pending approval";
    enableMock.mockRejectedValue(apiErrorFrom(409, { message: refusal }));
    renderTab();

    fireEvent.click(await screen.findByRole("button", { name: "+ Enable a repo" }));
    await screen.findByRole("option", { name: /org\/api/ });
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /featurly/ }));
    await screen.findByText(/Will deliver to:/);
    await screen.findByLabelText("Start only on these issue labels");
    fireEvent.click(screen.getByRole("button", { name: "Remove securite" }));

    fireEvent.click(screen.getByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[6]).toEqual([]);

    await screen.findByText(`API error 409: ${refusal}`);
    // Nothing was written, so the panel stays open on what was asked.
    expect(screen.getByText(/Label filter on this repo:/).textContent).toBe(
      "Label filter on this repo: securite — Enable removes it.",
    );
  });
});
