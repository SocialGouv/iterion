// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeConnections: vi.fn(async () => []),
    listForgeIntegrations: vi.fn(async () => []),
    listForgeOAuthApps: vi.fn(async () => []),
    registerForgeOAuthApp: vi.fn(async () => ({})),
    deleteForgeOAuthApp: vi.fn(async () => {}),
    listForgeRepos: vi.fn(async () => []),
    previewForgeEnable: vi.fn(async () => ({})),
    applyForgeConnectionAvatar: vi.fn(async () => ({})),
  };
});

vi.mock("@/api/bots", async () => {
  const actual = await vi.importActual<typeof import("@/api/bots")>("@/api/bots");
  return { ...actual, listBots: vi.fn(async () => []) };
});

import * as forgeApi from "@/api/forgeConnections";
import IntegrationsTab from "../IntegrationsTab";

// The tab reads its forge lists through react-query — give each render a
// fresh client (retry off so a mock failure surfaces immediately).
const renderTab = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <IntegrationsTab teamID="t1" canManage />
    </QueryClientProvider>,
  );

describe("IntegrationsTab — OAuth apps", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  // The legacy OAuth-apps + connect forms live behind the collapsed
  // "Manual setup (advanced)" disclosure — open it before asserting.
  const openManualSetup = async () => {
    fireEvent.click(await screen.findByRole("button", { name: /Manual setup \(advanced\)/ }));
  };

  it("renders the OAuth apps section with an empty state", async () => {
    renderTab();
    await openManualSetup();
    await screen.findByText("Forge OAuth apps");
    await screen.findByText("No OAuth app registered yet.");
  });

  it("registers an app via the default auto (admin-token) flow", async () => {
    renderTab();
    await openManualSetup();
    fireEvent.click(await screen.findByText("+ Register an OAuth app"));
    // Default mode is auto → an admin-token field is shown.
    const tokenInput = await screen.findByPlaceholderText(/Admin token/i);
    fireEvent.change(tokenInput, { target: { value: "admintok" } });
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() =>
      expect(forgeApi.registerForgeOAuthApp).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ provider: "gitlab", mode: "auto", admin_token: "admintok" }),
      ),
    );
  });

  it("registers an app via the manual (paste credentials) flow", async () => {
    renderTab();
    await openManualSetup();
    fireEvent.click(await screen.findByText("+ Register an OAuth app"));
    fireEvent.click(screen.getByLabelText("Paste credentials"));
    fireEvent.change(await screen.findByPlaceholderText(/Client ID/i), {
      target: { value: "cid" },
    });
    fireEvent.change(screen.getByPlaceholderText("Client secret"), {
      target: { value: "sec" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() =>
      expect(forgeApi.registerForgeOAuthApp).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ mode: "manual", client_id: "cid", client_secret: "sec" }),
      ),
    );
  });
});

// The iterion-bot avatar row on a connection card: the apply action exists
// exactly where iterion can act — a PAT on GitLab/Forgejo — and a GitHub App
// gets the manual-upload link instead. An OAuth connection (a person's
// account) shows neither.
describe("ConnectionCard — iterion-bot avatar", () => {
  const baseConn = {
    id: "c1",
    tenant_id: "t1",
    provider: "gitlab",
    kind: "pat",
    status: "active",
    account_login: "group_1_bot_x",
    forge_base_url: "https://gitlab.example.com",
    created_by: "u1",
    created_at: "2026-09-05T10:00:00Z",
    updated_at: "2026-09-05T10:00:00Z",
  };
  const conn = (over: Record<string, unknown>) =>
    ({ ...baseConn, ...over }) as unknown as forgeApi.ForgeConnection;

  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  it("applies directly on a bot account", async () => {
    vi.mocked(forgeApi.listForgeConnections).mockResolvedValue([conn({ account_kind: "bot" })]);
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Apply iterion-bot avatar" }));
    await waitFor(() =>
      expect(forgeApi.applyForgeConnectionAvatar).toHaveBeenCalledWith("t1", "c1", { force: false }),
    );
  });

  it("shows the applied state and offers a re-apply", async () => {
    vi.mocked(forgeApi.listForgeConnections).mockResolvedValue([
      conn({ account_kind: "bot", avatar_applied_at: "2026-09-05T10:00:00Z" }),
    ]);
    renderTab();
    await screen.findByText(/iterion-bot avatar ·/);
    await screen.findByRole("button", { name: "Re-apply the avatar" });
  });

  it("keeps an earlier success visible when a later re-apply failed", async () => {
    vi.mocked(forgeApi.listForgeConnections).mockResolvedValue([
      conn({
        account_kind: "bot",
        avatar_applied_at: "2026-09-05T10:00:00Z",
        avatar_error: "gitlab: avatar rejected (HTTP 400)",
      }),
    ]);
    renderTab();
    await screen.findByText(/Last avatar upload failed: gitlab: avatar rejected/);
    await screen.findByText(/iterion-bot avatar ·/);
    expect(screen.queryByText(/avatar not applied/)).toBeNull();
  });

  it("names a forge refusal on the card", async () => {
    vi.mocked(forgeApi.listForgeConnections).mockResolvedValue([
      conn({ account_kind: "bot", avatar_error: "gitlab: avatar rejected (HTTP 400)" }),
    ]);
    renderTab();
    await screen.findByText(/avatar not applied: gitlab: avatar rejected/);
  });

  it("shows nothing on an OAuth connection — a person's account", async () => {
    vi.mocked(forgeApi.listForgeConnections).mockResolvedValue([
      conn({ kind: "oauth_app", account_login: "alice", account_kind: "user" }),
    ]);
    renderTab();
    await screen.findByText(/@alice/);
    expect(screen.queryByRole("button", { name: /avatar/i })).toBeNull();
    expect(screen.queryByText(/iterion-bot avatar/)).toBeNull();
  });

  it("links a GitHub App connection to the manual logo upload", async () => {
    vi.mocked(forgeApi.listForgeConnections).mockResolvedValue([
      conn({
        provider: "github",
        kind: "github_app",
        account_login: "iterion-forge-1234[bot]",
        account_kind: "installation",
        oauth_app_id: "app1",
        forge_base_url: "https://github.com",
      }),
    ]);
    vi.mocked(forgeApi.listForgeOAuthApps).mockResolvedValue([
      {
        id: "app1",
        tenant_id: "t1",
        provider: "github",
        client_id: "x",
        auto_created: true,
        logo_upload_url: "https://github.com/organizations/acme/settings/apps/iterion-forge-1234",
        created_by: "u1",
        created_at: "2026-09-05T10:00:00Z",
        updated_at: "2026-09-05T10:00:00Z",
      } as unknown as forgeApi.ForgeOAuthApp,
    ]);
    renderTab();
    const link = await screen.findByRole("link", { name: /Upload the iterion-bot logo/ });
    expect(link.getAttribute("href")).toBe(
      "https://github.com/organizations/acme/settings/apps/iterion-forge-1234",
    );
    expect(screen.queryByRole("button", { name: /avatar/i })).toBeNull();
  });
});
