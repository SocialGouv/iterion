// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import type { ForgeOAuthApp } from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return { ...actual, connectForge: vi.fn() };
});

import { connectForge } from "@/api/forgeConnections";

import NonGitHubAuthorize from "./NonGitHubAuthorize";

const connectMock = vi.mocked(connectForge);

// The PLATFORM app of the instance, as the team listing carries it: an OAuth
// the wizard must offer although the team registered nothing itself.
const platformApp = {
  id: "app-platform",
  tenant_id: "platform",
  provider: "gitlab",
  forge_base_url: "https://pic.example",
  client_id: "cid",
  platform: true,
  created_at: "2026-10-01T00:00:00Z",
} as unknown as ForgeOAuthApp;

const forgejoApp = {
  id: "app-forgejo",
  tenant_id: "t1",
  provider: "forgejo",
  forge_base_url: "https://pic.example",
  client_id: "fcid",
  created_at: "2026-10-01T00:00:00Z",
} as unknown as ForgeOAuthApp;

function mount(provider: "gitlab" | "forgejo" = "gitlab") {
  const onError = vi.fn();
  render(
    <NonGitHubAuthorize
      teamID="t1"
      provider={provider}
      baseURL="https://pic.example"
      oauthApps={[platformApp, forgejoApp]}
      onBack={vi.fn()}
      onError={onError}
      onPatConnected={vi.fn()}
    />,
  );
  return { onError };
}

beforeEach(() => {
  vi.clearAllMocks();
  // No authorize_url: the component would assign window.location, which
  // jsdom does not implement; the request shape is what this pins.
  connectMock.mockResolvedValue({});
});
afterEach(cleanup);

describe("NonGitHubAuthorize — the posting identity of a GitLab OAuth connection", () => {
  it("offers OAuth on an instance covered by a platform app, posting as a project bot by default", async () => {
    mount();
    const box = screen.getByRole("checkbox", { name: /dedicated project bot/ });
    expect((box as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Continue with OAuth" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(1));
    expect(connectMock.mock.calls[0]?.[1]).toMatchObject({
      provider: "gitlab",
      mode: "oauth",
      forge_base_url: "https://pic.example",
      bot_identity: "project_token",
    });
  });

  it("sends no bot identity once the operator unticks the project bot", async () => {
    mount();
    fireEvent.click(screen.getByRole("checkbox", { name: /dedicated project bot/ }));
    fireEvent.click(screen.getByRole("button", { name: "Continue with OAuth" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(1));
    expect(connectMock.mock.calls[0]?.[1]?.bot_identity).toBeUndefined();
  });

  it("never offers the project bot on a forge without a project-token API", async () => {
    mount("forgejo");
    // The OAuth block IS offered (a forgejo app covers the instance) — only
    // the identity choice must be absent, and the request carries none.
    expect(screen.queryByRole("checkbox", { name: /dedicated project bot/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Continue with OAuth" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(1));
    expect(connectMock.mock.calls[0]?.[1]).toMatchObject({ provider: "forgejo", mode: "oauth" });
    expect(connectMock.mock.calls[0]?.[1]?.bot_identity).toBeUndefined();
  });
});
