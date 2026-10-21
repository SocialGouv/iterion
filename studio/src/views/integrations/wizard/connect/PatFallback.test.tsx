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

import PatFallback from "./PatFallback";

const connectMock = vi.mocked(connectForge);

const gitlabApp = {
  id: "app-1",
  tenant_id: "t1",
  provider: "gitlab",
  forge_base_url: "https://pic.example",
  client_id: "cid",
  created_at: "2026-10-01T00:00:00Z",
} as unknown as ForgeOAuthApp;

function mount() {
  render(
    <PatFallback
      teamID="t1"
      provider="gitlab"
      baseURL="https://pic.example"
      oauthApps={[gitlabApp]}
      onError={vi.fn()}
      onConnected={vi.fn()}
      bare
    />,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  connectMock.mockResolvedValue({});
});
afterEach(cleanup);

// The third OAuth entry point (the provider step's "other methods" pocket)
// must stamp the same identity as the primary one: a connection's posting
// identity is decided at connect and never recomputed.
describe("PatFallback — the OAuth pocket on GitLab", () => {
  it("sends the project-bot identity by default", async () => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Continue with OAuth" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(1));
    expect(connectMock.mock.calls[0]?.[1]).toMatchObject({
      provider: "gitlab",
      mode: "oauth",
      bot_identity: "project_token",
    });
  });

  it("sends no identity once unticked, and never on the token path", async () => {
    mount();
    fireEvent.click(screen.getByRole("checkbox", { name: /dedicated project bot/ }));
    fireEvent.click(screen.getByRole("button", { name: "Continue with OAuth" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(1));
    expect(connectMock.mock.calls[0]?.[1]?.bot_identity).toBeUndefined();

    fireEvent.change(screen.getByPlaceholderText(/Personal access token/), {
      target: { value: "glpat-x" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Connect with token" }));
    await waitFor(() => expect(connectMock).toHaveBeenCalledTimes(2));
    expect(connectMock.mock.calls[1]?.[1]).toMatchObject({ mode: "pat" });
    expect(connectMock.mock.calls[1]?.[1]?.bot_identity).toBeUndefined();
  });
});
