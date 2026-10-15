// @vitest-environment jsdom
// The bind wizard is the OTHER surface that provisions: its Enable writes the
// repo's bots, which registers the hook — and on a repo already enabled, MOVES
// it to the connection's pinned base. Its Review step therefore names the
// address the forge will be told to deliver to, and says so from the same
// table the Enable panel reads, so one deployment cannot be described two ways.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import type { ForgeEnablePreview, ForgeTeamRepo } from "@/api/forgeConnections";

const search = "step=review&repo=c1%3A%3Aorg%2Fapi&bot=review-pr";

vi.mock("wouter", async () => {
  const actual = await vi.importActual<typeof import("wouter")>("wouter");
  return {
    ...actual,
    useSearch: () => search,
    useLocation: () => ["/integrations/bind", vi.fn()],
  };
});

vi.mock("@/auth/AuthContext", async () => {
  const actual = await vi.importActual<typeof import("@/auth/AuthContext")>("@/auth/AuthContext");
  return {
    ...actual,
    useAuth: () => ({
      activeTeam: { team_id: "t1", team_name: "Team" },
      user: { id: "u1" },
      orgs: [{ org_id: "o1", org_role: "admin", teams: [{ team_id: "t1", role: "admin" }] }],
    }),
  };
});

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listTeamForgeRepos: vi.fn(async () => [
      {
        connection_id: "c1",
        provider: "gitlab",
        repo_full_name: "org/api",
        integration_id: "i1",
        bot_ids: [],
        sync_issues_enabled: false,
      } as ForgeTeamRepo,
    ]),
    previewForgeEnable: vi.fn(),
    enableForgeRepoBots: vi.fn(),
  };
});

vi.mock("@/api/secrets", async () => {
  const actual = await vi.importActual<typeof import("@/api/secrets")>("@/api/secrets");
  return { ...actual, listTeamSecrets: vi.fn(async () => []) };
});

import { enableForgeRepoBots, previewForgeEnable } from "@/api/forgeConnections";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import BindBotWizard from "@/views/integrations/BindBotWizard";

const previewMock = vi.mocked(previewForgeEnable);
const enableMock = vi.mocked(enableForgeRepoBots);

const reviewBot = {
  name: "review-pr",
  display_name: "Revi",
  invocations: [{ kind: "forge", forge: { event: "merge_request" } }],
} as unknown as BotEntryWithSchema;

const preview = (over: Partial<ForgeEnablePreview> = {}): ForgeEnablePreview => ({
  events_normalized: ["merge_request"],
  forge_native_events: ["merge_request"],
  scopes: {},
  secrets: [],
  commands: [],
  identity: { handle: "iterion-bot", provider: "gitlab", base_url: "https://pic.sg" },
  gates_merges: true,
  hook_base_url: "https://iterion.fabrique.social.gouv.fr",
  conflicts: [],
  ...over,
});

const renderWizard = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <BindBotWizard />
    </QueryClientProvider>,
  );

beforeEach(() => {
  vi.clearAllMocks();
  useServerInfoStore.setState({ info: { mode: "cloud" } as never });
  useBotsStore.setState({ bots: [reviewBot], loading: false, error: null });
});

afterEach(cleanup);

describe("BindBotWizard — the Review step names where the hook will be delivered", () => {
  it("prints the address the enable will register", async () => {
    previewMock.mockResolvedValue(preview());
    renderWizard();

    await screen.findByText("Will deliver to:");
    screen.getByText("https://iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/…");
  });

  it("warns instead when the deployment has no public URL", async () => {
    previewMock.mockResolvedValue(preview({ hook_base_url: "" }));
    renderWizard();

    await screen.findByText(/no address — this deployment has no public URL/);
    // No relative path passed off as an address, and the pin the operator
    // needs is one click away.
    expect(screen.queryByText(/api\/webhooks\/gitlab/)).toBeNull();
    expect(screen.getByRole("link", { name: "the connection" }).getAttribute("href")).toBe(
      "/integrations",
    );
  });

  it("warns, quoting it, when the base is not an absolute http(s) URL", async () => {
    previewMock.mockResolvedValue(preview({ hook_base_url: "/hooks" }));
    renderWizard();

    await screen.findByText(
      /no address — "\/hooks" is not an absolute http\(s\) URL \(auth\.publicUrl \/ ITERION_PUBLIC_URL\)/,
    );
    expect(screen.queryByText("/hooks/api/webhooks/gitlab/…")).toBeNull();
  });

  it("refuses Enable, saying why, when the preview could not be read", async () => {
    // This Enable registers the hook — and on a repo already enabled, moves
    // it to the connection's pinned base. With no preview the screen can name
    // neither that address nor what the bot subscribes to, so it provisions
    // nothing (the Enable panel's rule, on the other provisioning surface).
    previewMock.mockRejectedValue(new Error("API error 502: upstream"));
    renderWizard();

    await screen.findByText(
      /What enabling this bot would provision could not be read \(API error 502: upstream\) — Enable stays out until it can\./,
    );
    const enableBtn = screen.getByRole("button", { name: /^Enable / }) as HTMLButtonElement;
    expect(enableBtn.disabled).toBe(true);
    fireEvent.click(enableBtn);
    expect(enableMock).not.toHaveBeenCalled();
    // And no address is announced from an answer that never came.
    expect(screen.queryByText(/Will deliver to:/)).toBeNull();
  });

  it("says nothing about an address a server that reports none never gave", async () => {
    // An older server: the field is absent, which is not "no public URL".
    previewMock.mockResolvedValue(
      preview({ hook_base_url: undefined as unknown as string }),
    );
    renderWizard();

    await screen.findByText("Will subscribe to");
    expect(screen.queryByText("Will deliver to:")).toBeNull();
    expect(screen.queryByText(/no address/)).toBeNull();
  });
});
