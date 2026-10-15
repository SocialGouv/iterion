// @vitest-environment jsdom
// The connect wizard has ONE error slot (useConnectWizard: a step error wins,
// a load failure shows when no step error does). EnableRepoPanel is shared
// with the Integrations tab and reports the EMPTY STRING into that slot at
// the start of every repo-list load, which the slot has to read as "no
// message" and let the load failure behind it through again. That is what
// this witness drives: the panel mounts on step 3 of the real wizard, its
// list load clears the slot, and the refused OAuth-apps query stays the only
// statement anything makes about a list the operator cannot see.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const navigate = vi.fn();
vi.mock("wouter", () => ({
  useSearch: () => "connected=c1",
  useLocation: () => ["/integrations/connect", navigate],
}));

// `orgs` and `user` are what the step's permission read (useCanManageTeam)
// consumes; the real context types both as always-present, so the double
// carries them rather than being kinder than the producer. An empty tree =
// a member, which this witness does not care about.
vi.mock("@/auth/AuthContext", async () => {
  const actual = await vi.importActual<typeof import("@/auth/AuthContext")>(
    "@/auth/AuthContext",
  );
  return {
    ...actual,
    useAuth: () => ({
      activeTeam: { team_id: "t1", team_name: "Team" },
      user: null,
      orgs: [],
    }),
  };
});

vi.mock("@/api/bots", async () => {
  const actual = await vi.importActual<typeof import("@/api/bots")>("@/api/bots");
  return { ...actual, listBots: vi.fn(async () => []) };
});

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
    listForgeOAuthApps: vi.fn(),
    listForgeRepos: vi.fn(async () => []),
    previewForgeEnable: vi.fn(async () => ({})),
  };
});

import { listForgeOAuthApps, listForgeRepos } from "@/api/forgeConnections";
import { useServerInfoStore } from "@/store/serverInfo";
import ConnectRepoWizard from "@/views/integrations/ConnectRepoWizard";

const appsMock = vi.mocked(listForgeOAuthApps);
const reposMock = vi.mocked(listForgeRepos);

const renderWizard = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <ConnectRepoWizard />
    </QueryClientProvider>,
  );

beforeEach(() => {
  vi.clearAllMocks();
  useServerInfoStore.setState({ info: { mode: "cloud" } as never });
  reposMock.mockResolvedValue([]);
});

afterEach(cleanup);

describe("ConnectRepoWizard — the one error slot", () => {
  it("still shows a load failure once the repo panel has loaded its list", async () => {
    appsMock.mockRejectedValue(new Error("503 service unavailable: list oauth apps"));
    renderWizard();

    // Step 3 is on screen and the panel has run its mount load: the list
    // query settled, so nothing is in flight behind the banner.
    await screen.findByText("Pick a repository");
    await waitFor(() => expect(reposMock).toHaveBeenCalled());
    await waitFor(() => expect(appsMock).toHaveBeenCalled());

    // The OAuth-apps list is broken; the wizard's banner is the only
    // statement anything makes about it.
    await screen.findByText(/503 service unavailable: list oauth apps/);
  });
});
