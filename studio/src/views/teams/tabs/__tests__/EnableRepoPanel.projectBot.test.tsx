// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import type { ForgeConnection, ForgeEnablePreview, ForgeRepo } from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeIntegrations: vi.fn(),
    listForgeRepos: vi.fn(),
    previewForgeEnable: vi.fn(),
    enableForgeRepoBots: vi.fn(),
  };
});

import {
  listForgeIntegrations,
  listForgeRepos,
  previewForgeEnable,
} from "@/api/forgeConnections";

import { EnableRepoPanel } from "../integrations/EnableRepoPanel";

const reposMock = vi.mocked(listForgeRepos);
const previewMock = vi.mocked(previewForgeEnable);
const integrationsMock = vi.mocked(listForgeIntegrations);

// A GitLab OAuth connection that posts as per-repository bots: the preview
// names no account of its own before the first enable.
const conn = {
  id: "c1",
  provider: "gitlab",
  kind: "oauth_app",
  status: "active",
  account_login: "alice",
  bot_identity: "project_token",
  created_at: "2026-10-01T00:00:00Z",
} as ForgeConnection;

const reviewBot = {
  name: "review-pr",
  display_name: "Revi",
  invocations: [{ kind: "forge", forge: { event: "merge_request" } }],
} as unknown as BotEntryWithSchema;

const preview = (identity: ForgeEnablePreview["identity"]): ForgeEnablePreview => ({
  events_normalized: ["merge_request"],
  forge_native_events: ["merge_request"],
  scopes: {},
  secrets: [],
  commands: [],
  identity,
  gates_merges: true,
  hook_base_url: "https://iterion.fabrique.social.gouv.fr",
  conflicts: [],
});

function renderPanel() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <EnableRepoPanel
        teamID="t1"
        conn={conn}
        repoBots={[reviewBot]}
        onDone={vi.fn()}
        onCancel={vi.fn()}
        onError={vi.fn()}
      />
    </QueryClientProvider>,
  );
}

async function selectRepoAndBot() {
  await screen.findByRole("option", { name: /org\/api/ });
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
  fireEvent.click(screen.getByRole("checkbox"));
  await waitFor(() => expect(previewMock).toHaveBeenCalled());
}

beforeEach(() => {
  vi.clearAllMocks();
  reposMock.mockResolvedValue([{ full_name: "org/api", can_admin: true } as ForgeRepo]);
  integrationsMock.mockResolvedValue([]);
});
afterEach(cleanup);

describe("EnableRepoPanel — who a project-bot connection posts as", () => {
  it("announces the bot iterion will create, never the authorising person", async () => {
    previewMock.mockResolvedValue(
      preview({ handle: "", provider: "gitlab", base_url: "https://pic.example", project_bot: true }),
    );
    renderPanel();
    await selectRepoAndBot();
    await screen.findByText(/Will post as:/);
    screen.getByText(/a dedicated project bot \(iterion-bot\) that iterion creates on this repository/);
    expect(screen.queryByText(/@alice/)).toBeNull();
  });

  it("names the minted bot once the repository has one", async () => {
    previewMock.mockResolvedValue(
      preview({
        handle: "project_194_bot_a7c08cc4",
        provider: "gitlab",
        base_url: "https://pic.example",
        project_bot: true,
      }),
    );
    renderPanel();
    await selectRepoAndBot();
    await screen.findByText(/@project_194_bot_a7c08cc4/);
    screen.getByText(/this repository's bot, created by iterion/);
  });
});
