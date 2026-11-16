// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

let search = "";
const setLocation = vi.fn();
vi.mock("wouter", () => ({
  useSearch: () => search,
  useLocation: () => ["/sources/plug", setLocation],
  Link: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ activeTeam: { team_id: "t1" } }),
}));

const listSources = vi.fn();
const listConnections = vi.fn();
const listBots = vi.fn();
const previewTrigger = vi.fn();
const createTrigger = vi.fn();
const listWebhooks = vi.fn();
const createWebhook = vi.fn();

vi.mock("@/api/sources", () => ({
  listSources: (...a: unknown[]) => listSources(...a),
  sourceIconURL: (id: string) => `/api/v1/connectors/${id}/icon`,
  sourceCategoryLabel: (c?: string) => c ?? "Autres",
}));
vi.mock("@/api/connections", () => ({
  listConnections: (...a: unknown[]) => listConnections(...a),
}));
vi.mock("@/api/bots", () => ({
  listBots: (...a: unknown[]) => listBots(...a),
}));
vi.mock("@/api/triggers", () => ({
  createTrigger: (...a: unknown[]) => createTrigger(...a),
  previewTrigger: (...a: unknown[]) => previewTrigger(...a),
}));
vi.mock("@/api/webhooks", () => ({
  createWebhook: (...a: unknown[]) => createWebhook(...a),
  listWebhooks: (...a: unknown[]) => listWebhooks(...a),
}));

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import PlugWizardView from "./PlugWizardView";

function renderWizard() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <PlugWizardView />
    </QueryClientProvider>,
  );
}
import type { Source } from "@/api/sources";

const SOURCE: Source = {
  kind: "connector",
  id: "probe",
  display_name: "Probe",
  category: "source_control",
  maturity: "experimental",
  tier: "team",
  delivery: "push",
  connections: 1,
  subscriptions: 0,
  triggers: [
    {
      id: "probe.issue.commented",
      display_name: "Issue comment",
      outputs: [{ name: "prompt", type: "string" }],
      filters: [{ id: "author", type: "enum", options: ["alice", "bob"] }],
    },
  ],
};

beforeEach(() => {
  vi.clearAllMocks();
  search = "?source=probe&step=account";
  listSources.mockResolvedValue([SOURCE]);
  listConnections.mockResolvedValue([
    { id: "conn-1", alias: "main", base_url: "https://probe.example", connector: "probe" },
  ]);
  listBots.mockResolvedValue([{ name: "steward" }]);
  listWebhooks.mockResolvedValue([]);
  createWebhook.mockResolvedValue({
    config: { id: "wh-1", provider: "connector" },
    token: "one-time-secret",
  });
  createTrigger.mockResolvedValue({});
  previewTrigger.mockResolvedValue({ matched: true, sample: true, vars: { prompt: "ship it" } });
});

afterEach(cleanup);

describe("PlugWizardView", () => {
  it("refuses an unknown source", async () => {
    search = "?source=nope";
    renderWizard();
    await waitFor(() => screen.getByText("Source inconnue"));
    expect(createTrigger).not.toHaveBeenCalled();
  });

  it("carries the picked connection into the event step's URL", async () => {
    renderWizard();
    await screen.findByText("main");
    fireEvent.click(screen.getByRole("radio", { name: /main/ }));
    expect(setLocation).toHaveBeenCalledWith(
      "/sources/plug?source=probe&step=event&connection=conn-1",
      { replace: true },
    );
  });

  it("previews the sample through the lane's own judgment at the mapping step", async () => {
    search = "?source=probe&step=mapping&connection=conn-1&trigger=probe.issue.commented&bot=steward";
    renderWizard();
    await screen.findByPlaceholderText("prompt");
    fireEvent.click(screen.getByRole("button", { name: /Prévisualiser/ }));
    await waitFor(() => screen.getByText(/Correspond/));
    expect(previewTrigger).toHaveBeenCalledWith({
      bot_id: "steward",
      connection_id: "conn-1",
      trigger_id: "probe.issue.commented",
      filters: {},
      var_map: {},
    });
  });

  it("enables with the connector binding and the minted webhook", async () => {
    search = "?source=probe&step=review&connection=conn-1&trigger=probe.issue.commented&bot=steward";
    renderWizard();
    fireEvent.click(await screen.findByRole("button", { name: /Activer/ }));
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    expect(createWebhook).toHaveBeenCalledWith("t1", {
      name: "Probe — probe.issue.commented",
      provider: "connector",
      connector_connection_id: "conn-1",
    });
    expect(createTrigger).toHaveBeenCalledWith(
      expect.objectContaining({
        bot_id: "steward",
        match: {},
        invocation: "connector",
        connection_id: "conn-1",
        trigger_id: "probe.issue.commented",
        enabled: true,
      }),
    );
  });

  it("reuses the connection's webhook instead of minting a second one", async () => {
    listWebhooks.mockResolvedValue([{ id: "wh-existing", connector_connection_id: "conn-1" }]);
    search = "?source=probe&step=review&connection=conn-1&trigger=probe.issue.commented&bot=steward";
    renderWizard();
    fireEvent.click(await screen.findByRole("button", { name: /Activer/ }));
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    expect(createWebhook).not.toHaveBeenCalled();
  });
});

describe("PlugWizardView — the bot page's entry", () => {
  it("picks a source first and keeps the bot it carried", async () => {
    search = "?bot=steward";
    renderWizard();
    const card = await screen.findByRole("radio", { name: /Probe/ });
    fireEvent.click(card);
    expect(setLocation).toHaveBeenCalledWith(
      "/sources/plug?bot=steward&source=probe&step=account",
      { replace: true },
    );
  });
});
