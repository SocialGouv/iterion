// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import type { ForgeOAuthApp } from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return { ...actual, connectForge: vi.fn() };
});
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) =>
    sel({ info: { forge_github_app_configured: false } }),
}));

import { connectForge } from "@/api/forgeConnections";

import { ConnectForm } from "../integrations/ConnectForm";

const connectMock = vi.mocked(connectForge);

const gitlabApp = {
  id: "app-1",
  tenant_id: "t1",
  provider: "gitlab",
  client_id: "cid",
  created_at: "2026-10-01T00:00:00Z",
} as unknown as ForgeOAuthApp;

beforeEach(() => {
  vi.clearAllMocks();
  connectMock.mockResolvedValue({});
});
afterEach(cleanup);

// The team tab's own connect form is the second OAuth entry point: the same
// identity choice, the same request.
describe("ConnectForm — GitLab OAuth posting identity", () => {
  it("sends the project-bot identity by default on GitLab OAuth, and none once unticked", async () => {
    render(<ConnectForm teamID="t1" oauthApps={[gitlabApp]} onConnected={vi.fn()} onError={vi.fn()} />);
    const oauth = screen.getByRole("radio", { name: /Use OAuth/ });
    expect((oauth as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(1));
    expect(connectMock.mock.calls[0]?.[1]).toMatchObject({
      provider: "gitlab",
      mode: "oauth",
      bot_identity: "project_token",
    });

    fireEvent.click(screen.getByRole("checkbox", { name: /dedicated project bot/ }));
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(2));
    expect(connectMock.mock.calls[1]?.[1]?.bot_identity).toBeUndefined();
  });

  it("offers no identity choice on the token path", () => {
    render(<ConnectForm teamID="t1" oauthApps={[gitlabApp]} onConnected={vi.fn()} onError={vi.fn()} />);
    fireEvent.click(screen.getByRole("radio", { name: /Paste a token/ }));
    expect(screen.queryByRole("checkbox", { name: /dedicated project bot/ })).toBeNull();
  });
});
