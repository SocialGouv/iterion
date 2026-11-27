// @vitest-environment jsdom
// Revoking a personal access token ends the pending invitations it emailed
// (ADR-174): the panel names them so the operator knows who to invite again,
// and says nothing more when the revoke ended none.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/api/pats", async () => {
  const actual = await vi.importActual<typeof import("@/api/pats")>("@/api/pats");
  return {
    ...actual,
    listMyTokens: vi.fn(async () => [
      {
        id: "p1",
        name: "cli",
        team_id: "ta",
        token_last4: "abcd",
        created_at: "2026-11-01T00:00:00Z",
        user_id: "u",
      },
    ]),
    revokeMyToken: vi.fn(async () => ({
      invitations_revoked: [{ id: "i1", email: "lucas@example.org", team_id: "ta", role: "admin" }],
    })),
  };
});

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ teams: [{ team_id: "ta", team_name: "Egapro" }] }),
}));

import TokensPanel from "@/views/account/TokensPanel";
import { revokeMyToken } from "@/api/pats";

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  cleanup();
});

function renderPanel(): void {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <TokensPanel />
    </QueryClientProvider>,
  );
}

async function revokeTheToken(): Promise<void> {
  const row = (await screen.findByText("cli", {}, { timeout: 8000 })).closest("tr")!;
  fireEvent.click(within(row).getByRole("button", { name: "Revoke cli" }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(/pending invitations it sent by email end with it/)).toBeTruthy();
  fireEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
}

describe("revoking a personal access token", () => {
  it("names the invitations the revoke ended", async () => {
    renderPanel();
    await revokeTheToken();
    expect(await screen.findByText("Revoking cli ended the invitations it sent", {}, { timeout: 8000 })).toBeTruthy();
    expect(screen.getByText("lucas@example.org — Egapro, admin")).toBeTruthy();
    expect(vi.mocked(revokeMyToken)).toHaveBeenCalledWith("p1");
  });

  it("says nothing more when the revoke ended none", async () => {
    vi.mocked(revokeMyToken).mockResolvedValueOnce({ invitations_revoked: [] });
    renderPanel();
    await revokeTheToken();
    await vi.waitFor(() => expect(vi.mocked(revokeMyToken)).toHaveBeenCalledWith("p1"));
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByText(/ended the invitations it sent/)).toBeNull();
  });

  it("keeps an earlier list up when a later revoke ended nothing", async () => {
    renderPanel();
    await revokeTheToken();
    await screen.findByText("lucas@example.org — Egapro, admin", {}, { timeout: 8000 });
    vi.mocked(revokeMyToken).mockResolvedValueOnce({ invitations_revoked: [] });
    await revokeTheToken();
    await vi.waitFor(() => expect(vi.mocked(revokeMyToken)).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText("lucas@example.org — Egapro, admin")).toBeTruthy();
  });

  it("does not claim the invitations ended when the sweep could not list them", async () => {
    vi.mocked(revokeMyToken).mockResolvedValueOnce({ invitations_revoked: [], invitations_error: "listing failed: re-issue the revoke" });
    renderPanel();
    await revokeTheToken();
    expect(await screen.findByText("Revoking cli could not end every invitation it sent", {}, { timeout: 8000 })).toBeTruthy();
    expect(screen.getByText("listing failed: re-issue the revoke")).toBeTruthy();
    expect(screen.queryByText(/ended the invitations it sent/)).toBeNull();
  });
});
