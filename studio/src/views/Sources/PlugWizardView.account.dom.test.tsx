// @vitest-environment jsdom
// The account step's way out to the Connectors tab, driven through the real
// router mounted under the studio's base: the URL it lands on is the effect.
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ activeTeam: { team_id: "t1" } }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (m: { info?: { mode?: string } }) => unknown) =>
    sel({ info: { mode: "cloud" } }),
}));

const listSources = vi.fn();
const listConnections = vi.fn();
vi.mock("@/api/sources", async () => {
  const actual = await vi.importActual<typeof import("@/api/sources")>("@/api/sources");
  return {
    ...actual,
    listSources: (...a: unknown[]) => listSources(...a),
    sourceIconURL: (id: string) => `/api/v1/connectors/${id}/icon`,
  };
});
vi.mock("@/api/connections", () => ({
  listConnections: (...a: unknown[]) => listConnections(...a),
  patchConnection: vi.fn(),
}));
vi.mock("@/api/bots", () => ({ listBots: vi.fn(async () => []) }));
vi.mock("@/api/triggers", () => ({ createTrigger: vi.fn(), previewTrigger: vi.fn() }));
vi.mock("@/api/webhooks", () => ({ createWebhook: vi.fn(), listWebhooks: vi.fn() }));

import PlugWizardView from "./PlugWizardView";
import type { Source } from "@/api/sources";

const JIRA: Source = {
  kind: "connector",
  id: "jira",
  display_name: "Jira",
  delivery: "mixed",
  connections: 0,
  subscriptions: 0,
  triggers: [{ id: "jira.issue.updated", display_name: "Issue updated", delivery: "push" }],
};

// Where the Connectors tab must send the operator back: the wizard's own
// URL, every choice it carried kept.
const BACK = "/sources/plug?source=jira&step=account&bot=steward";
const TAB =
  "/studio/integrations?tab=connectors&connector=jira&returnTo=" + encodeURIComponent(BACK);

function renderAt(path: string) {
  const { hook, history } = memoryLocation({ path, record: true });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <Router base="/studio" hook={hook}>
        <PlugWizardView />
      </Router>
    </QueryClientProvider>,
  );
  return history as string[];
}

beforeEach(() => {
  vi.clearAllMocks();
  listSources.mockResolvedValue([JIRA]);
});

afterEach(cleanup);

describe("PlugWizardView — the account step's way to a new connection", () => {
  it("opens the Connectors tab on the source's connector, under the studio's base, with the way back", async () => {
    listConnections.mockResolvedValue([]);
    const history = renderAt(`/studio${BACK}`);
    fireEvent.click(await screen.findByRole("button", { name: "Créer une connexion" }));
    expect(history[history.length - 1]).toBe(TAB);
  });

  it("still offers another connection when the connector already has one", async () => {
    listConnections.mockResolvedValue([
      { id: "conn-1", alias: "main", connector: "jira", capabilities: ["trigger"] },
    ]);
    const history = renderAt(`/studio${BACK}`);
    fireEvent.click(await screen.findByRole("link", { name: "Créer une autre connexion Jira" }));
    expect(history[history.length - 1]).toBe(TAB);
  });
});
