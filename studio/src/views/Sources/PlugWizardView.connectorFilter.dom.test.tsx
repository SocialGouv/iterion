// @vitest-environment jsdom
// The account step lists only the source's own connections (#3439). The
// connections client is the REAL one: only the network is doubled, by a
// server that filters on the `connector` query parameter the way
// pkg/server's connection listing does. A client that drops the parameter
// gets the whole team's connections back, and the step shows them.
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type { Source } from "@/api/sources";

vi.mock("wouter", () => ({
  useSearch: () => "?source=rss&step=account",
  useLocation: () => ["/sources/plug", vi.fn()],
  Link: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ activeTeam: { team_id: "t1" } }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (m: { info?: { mode?: string } }) => unknown) =>
    sel({ info: { mode: "cloud" } }),
}));

const RSS: Source = {
  kind: "connector",
  id: "rss",
  display_name: "RSS",
  delivery: "poll",
  connections: 1,
  subscriptions: 0,
  triggers: [],
};

vi.mock("@/api/sources", async () => {
  const actual = await vi.importActual<typeof import("@/api/sources")>("@/api/sources");
  return {
    ...actual,
    listSources: async () => [RSS],
    sourceIconURL: (id: string) => `/api/v1/connectors/${id}/icon`,
  };
});
vi.mock("@/api/bots", () => ({ listBots: async () => [] }));
vi.mock("@/api/triggers", () => ({ createTrigger: vi.fn(), previewTrigger: vi.fn() }));
vi.mock("@/api/webhooks", () => ({ createWebhook: vi.fn(), listWebhooks: async () => [] }));

import PlugWizardView from "./PlugWizardView";

// The team holds one connection per connector.
const TEAM_CONNECTIONS = [
  { id: "c-rss", alias: "veille", connector: "rss", base_url: "https://feed.example" },
  { id: "c-jira", alias: "tickets", connector: "jira", base_url: "https://jira.example" },
];

let listed: string[] = [];

beforeEach(() => {
  listed = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request) => {
      const url = new URL(String(input), "http://studio.test");
      if (url.pathname !== "/api/teams/t1/connections") {
        return new Response("not found", { status: 404 });
      }
      listed.push(url.search);
      const connector = url.searchParams.get("connector");
      const connections = TEAM_CONNECTIONS.filter((c) => !connector || c.connector === connector);
      return new Response(JSON.stringify({ connections }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("lists only the source's connections at the account step", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <PlugWizardView />
    </QueryClientProvider>,
  );
  const group = await screen.findByRole("radiogroup", { name: "Connexion" });
  const offered = Array.from(group.querySelectorAll('[role="radio"]'), (el) => el.textContent);
  expect(offered).toEqual(["veille — https://feed.example"]);
  expect(listed).toEqual(["?connector=rss"]);
});
