// @vitest-environment jsdom
// The wizard's state machine is the URL. After Enable it navigates to
// ?step=done&connected=<id>&repo=…, and `connected` is also what an OAuth /
// App-install round-trip comes back with — read as one, that URL sends the
// operator back to "Repositories" and the Done step never renders, right
// after the one action that earns it. This witness drives the real chain: the
// enable, the URL the wizard itself picks, and the step that URL produces.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import type { ForgeEnablePreview, ForgeRepo } from "@/api/forgeConnections";

let search = "connected=c1";
const navigate = vi.fn();

vi.mock("wouter", async () => {
  const actual = await vi.importActual<typeof import("wouter")>("wouter");
  return {
    ...actual,
    useSearch: () => search,
    useLocation: () => ["/integrations/connect", navigate],
  };
});

vi.mock("@/auth/AuthContext", async () => {
  const actual = await vi.importActual<typeof import("@/auth/AuthContext")>(
    "@/auth/AuthContext",
  );
  return {
    ...actual,
    // An admin of the team: what the connection row's pin affordance needs.
    useAuth: () => ({
      activeTeam: { team_id: "t1", team_name: "Team" },
      user: { id: "u1" },
      orgs: [
        {
          org_id: "o1",
          org_role: "admin",
          teams: [{ team_id: "t1", role: "admin" }],
        },
      ],
    }),
  };
});

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
    listForgeOAuthApps: vi.fn(async () => []),
    listForgeRepos: vi.fn(async () => [{ full_name: "org/api", can_admin: true } as ForgeRepo]),
    previewForgeEnable: vi.fn(
      async (): Promise<ForgeEnablePreview> => ({
        events_normalized: ["merge_request"],
        forge_native_events: ["merge_request"],
        scopes: {},
        secrets: [],
        commands: [],
        identity: { handle: "iterion-bot", provider: "gitlab", base_url: "https://gitlab.example" },
        gates_merges: false,
        hook_base_url: "https://iterion.example.org",
        conflicts: [],
      }),
    ),
    enableForgeRepoBots: vi.fn(async () => ({
      integration_id: "i1",
      webhook_id: "w1",
      hook_id: "h1",
      managed_secret_id: "s1",
      bot_ids: ["review-pr"],
      created: true,
    })),
  };
});

import { enableForgeRepoBots } from "@/api/forgeConnections";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import ConnectRepoWizard from "@/views/integrations/ConnectRepoWizard";

const enableMock = vi.mocked(enableForgeRepoBots);

const reviewBot = {
  name: "review-pr",
  display_name: "Revi",
  invocations: [{ kind: "forge", forge: { event: "merge_request" } }],
} as unknown as BotEntryWithSchema;

const renderWizard = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <ConnectRepoWizard />
    </QueryClientProvider>,
  );

beforeEach(() => {
  navigate.mockClear();
  enableMock.mockClear();
  search = "connected=c1";
  useServerInfoStore.setState({ info: { mode: "cloud" } as never });
  // The catalog the Repositories step offers; seeded so the store's one-shot
  // fetch is a no-op.
  useBotsStore.setState({ bots: [reviewBot], loading: false, error: null });
});

afterEach(cleanup);

describe("ConnectRepoWizard — which step a URL produces", () => {
  it("lands on Done with the very URL it navigates to after Enable", async () => {
    renderWizard();

    await screen.findByText("Pick a repository");
    await screen.findByRole("option", { name: /org\/api/ });
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
    fireEvent.click(screen.getByRole("checkbox"));
    // The panel refuses to provision before the preview for this selection
    // has landed: the address the hook is registered under comes from it.
    await screen.findByText("https://iterion.example.org/api/webhooks/gitlab/…");
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));

    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(navigate).toHaveBeenCalledTimes(1));

    // Not a URL this test invents: the one the wizard chose.
    const target = String(navigate.mock.calls[0]?.[0]);
    expect(target).toContain("step=done");
    expect(target).toContain("connected=c1");
    search = target.split("?")[1] ?? "";

    cleanup();
    renderWizard();
    await screen.findByText("Repository connected");
    // And it names the repo that was just enabled.
    screen.getByText("org/api");
  });

  it("offers the hook delivery base ABOVE the Enable panel", async () => {
    renderWizard();

    // The hook is registered BY the enable, so the base a refusing forge
    // needs must be pinnable before the operator reaches the button — and by
    // this admin, who holds the connection.
    const delivery = await screen.findByText("Webhook delivery:");
    screen.getByRole("button", { name: "Pin a base URL" });
    const panel = screen.getByLabelText("Search repos");
    expect(
      delivery.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the row must come before the panel it is a precondition of",
    ).toBeTruthy();
  });

  it("still forces Repositories when only `connected` is in the URL", async () => {
    search = "connected=c1";
    renderWizard();

    // The round-trip case: the connection is fresh and no repo is enabled
    // yet, so the operator belongs on step 3.
    await screen.findByText("Pick a repository");
    expect(screen.queryByText("Repository connected")).toBeNull();
  });
});
