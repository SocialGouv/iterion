// @vitest-environment jsdom
// The Integrations tab has ONE error banner, fed by every mutation of every
// child section through `onError`. A handler whose success changes an
// integration row takes it down through `onChanged` → `reload()`; the three
// that do NOT — a sync changes no row, a repo-list load changes nothing, an
// App install succeeds into another tab — have to clear the channel
// themselves, where the attempt starts, or they leave the banner claiming a
// failure the next attempt already replaced, next to that attempt's own green
// result. These witnesses drive the REAL tab and its REAL children, one per
// handler of that class, plus the control that has the clearing path.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

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
    listForgeIntegrations: vi.fn(async () => [
      {
        id: "i1",
        connection_id: "c1",
        provider: "github",
        repo_full_name: "org/api",
        bot_ids: ["triage"],
        sync_issues_enabled: true,
      },
    ]),
    listForgeOAuthApps: vi.fn(async () => []),
    listForgeRepos: vi.fn(),
    previewForgeEnable: vi.fn(async () => ({})),
    applyForgeConnectionAvatar: vi.fn(async () => ({})),
    listForgeIntegrationHooks: vi.fn(async () => []),
    syncForgeIntegration: vi.fn(),
    updateForgeIntegration: vi.fn(),
    connectForge: vi.fn(),
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
  return { ...actual, listBots: vi.fn(async () => []) };
});

import {
  connectForge,
  listForgeOAuthApps,
  listForgeRepos,
  syncForgeIntegration,
  updateForgeIntegration,
} from "@/api/forgeConnections";
import IntegrationsTab from "../IntegrationsTab";

const syncMock = vi.mocked(syncForgeIntegration);
const reposMock = vi.mocked(listForgeRepos);
const toggleMock = vi.mocked(updateForgeIntegration);
const appsMock = vi.mocked(listForgeOAuthApps);
const connectMock = vi.mocked(connectForge);

// The tab reads its forge lists through react-query — a fresh client per
// render, retry off so a mock failure surfaces immediately.
const renderTab = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <IntegrationsTab teamID="t1" canManage />
    </QueryClientProvider>,
  );

describe("IntegrationsTab — the banner says what is true NOW", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    appsMock.mockResolvedValue([]);
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("drops a refused sync's banner once a later pass mirrored the window", async () => {
    renderTab();
    const syncNow = await screen.findByTitle("Pull the forge's issues onto the board now");

    // The read was cut short: the endpoint refuses the whole pass (502) and
    // holds the mark. That refusal is the only statement about the missing
    // window, and the operator must see it.
    syncMock.mockRejectedValueOnce(
      new Error(
        "sync failed: list issues: 20000 entries read in 200 pages and the forge has not " +
          "reported the window exhausted",
      ),
    );
    fireEvent.click(syncNow);
    await waitFor(() => expect(syncMock).toHaveBeenCalledTimes(1));
    await screen.findByText(/200 pages/);

    // The operator retries and the whole window lands.
    syncMock.mockResolvedValueOnce({ synced: 250, created: 250, updated: 0 });
    fireEvent.click(syncNow);
    await waitFor(() => expect(syncMock).toHaveBeenCalledTimes(2));
    await screen.findByText(/Synced 250 issues/);

    expect(
      screen.queryByText(/200 pages/),
      "the refusal of a pass a later one replaced is still on screen, beside that pass's green counts",
    ).toBeNull();
  });

  it("drops a refused repo list's banner once a later search loaded it", async () => {
    reposMock.mockRejectedValueOnce(new Error("list repos: 403 forbidden for this token"));
    reposMock.mockResolvedValue([{ full_name: "org/api" }] as never);
    renderTab();
    fireEvent.click(await screen.findByText("+ Enable a repo"));
    await screen.findByText(/403 forbidden/);

    fireEvent.click(screen.getByText("Search"));
    await waitFor(() => expect(reposMock).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryAllByText(/org\/api/).length).toBeGreaterThan(0));

    expect(
      screen.queryByText(/403 forbidden/),
      "the repo list that failed once still claims it while the repos a later search loaded are on screen",
    ).toBeNull();
  });

  it("drops a refused App install's banner once a later one handed off to its tab", async () => {
    appsMock.mockResolvedValue([
      {
        id: "a1",
        provider: "github",
        forge_base_url: "https://github.com",
        installable: true,
        created_at: "2026-10-01T00:00:00Z",
      },
    ] as never);
    connectMock.mockRejectedValueOnce(new Error("connect forge: 500 app manifest is gone"));
    connectMock.mockResolvedValue({
      install_url: "https://github.com/apps/x/installations/new",
    } as never);
    // The hand-off opens a NEW tab, so this one stays put with whatever the
    // banner holds — the reason nothing else clears it here.
    const opened = vi.fn(() => ({}) as Window);
    vi.stubGlobal("open", opened);
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: /Manual setup \(advanced\)/ }));
    const installBtn = await screen.findByTitle(/Install this App on GitHub/);
    fireEvent.click(installBtn);
    await screen.findByText(/500 app manifest is gone/);

    fireEvent.click(installBtn);
    await waitFor(() => expect(opened).toHaveBeenCalledTimes(1));

    expect(
      screen.queryByText(/500 app manifest is gone/),
      "the install that failed once still claims it while the retry opened its install tab",
    ).toBeNull();
  });

  // The other half of the fix: the handlers above clear the channel with the
  // empty string, which has to HIDE the banner and leave the queries' own
  // failures visible. Reading that channel with `??` would make an empty
  // string a message and swallow a load failure for the rest of the session.
  it("keeps a load failure visible after an attempt cleared the action channel", async () => {
    appsMock.mockRejectedValue(new Error("list oauth apps: 503 store unavailable"));
    renderTab();
    await screen.findByText(/503 store unavailable/);

    // A sync that SUCCEEDS: it clears the channel on its way in and never
    // writes to it again.
    syncMock.mockResolvedValueOnce({ synced: 3, created: 0, updated: 0 });
    fireEvent.click(await screen.findByTitle("Pull the forge's issues onto the board now"));
    await waitFor(() => expect(syncMock).toHaveBeenCalledTimes(1));
    await screen.findByText(/Synced 3 issues/);

    await screen.findByText(/503 store unavailable/);
  });

  // The control: a handler WITH a clearing path needs no clear of its own.
  // Its success calls onChanged → reload(), which empties the banner — which
  // is why the fix above belongs only to the handlers that never get there.
  it("needs no clear on the sync toggle, whose success already reloads the tab", async () => {
    toggleMock.mockRejectedValueOnce(new Error("patch integration: 409 webhook is gone"));
    toggleMock.mockResolvedValue({
      id: "i1",
      connection_id: "c1",
      provider: "github",
      repo_full_name: "org/api",
      bot_ids: ["triage"],
      sync_issues_enabled: false,
    } as never);
    renderTab();
    const box = await screen.findByLabelText("Sync issues to board");
    fireEvent.click(box);
    await screen.findByText(/409 webhook is gone/);

    fireEvent.click(box);
    await waitFor(() => expect(toggleMock).toHaveBeenCalledTimes(2));

    expect(
      screen.queryByText(/409 webhook is gone/),
      "the toggle's success path reaches onChanged, so the banner it fed must be gone without the handler clearing it",
    ).toBeNull();
  });
});
