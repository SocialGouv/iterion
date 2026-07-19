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
