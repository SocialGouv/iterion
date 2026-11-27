// @vitest-environment jsdom
// The token lifecycle in the panel: a status per row, the acts each status
// allows (regenerate, disable/enable, revoke — delete on a revoked row only),
// the "Show revoked" toggle asking the server for the dead tokens, and a
// regenerate showing its new plaintext once and naming what it ended.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const live = {
  id: "p-live",
  name: "cli",
  team_id: "ta",
  token_last4: "aaaa",
  created_at: "2026-11-01T00:00:00Z",
  user_id: "u",
};
const off = { ...live, id: "p-off", name: "laptop", token_last4: "bbbb", disabled_at: "2026-11-02T00:00:00Z" };
const dead = { ...live, id: "p-dead", name: "old", token_last4: "cccc", revoked_at: "2026-11-03T00:00:00Z" };

vi.mock("@/api/pats", async () => {
  const actual = await vi.importActual<typeof import("@/api/pats")>("@/api/pats");
  return {
    ...actual,
    listMyTokens: vi.fn(async (opts?: { includeRevoked?: boolean }) =>
      opts?.includeRevoked ? [live, off, dead] : [live, off],
    ),
    disableMyToken: vi.fn(async () => ({ ...live, disabled_at: "2026-11-04T00:00:00Z" })),
    enableMyToken: vi.fn(async () => ({ ...off, disabled_at: undefined })),
    purgeMyToken: vi.fn(async () => ({ invitations_revoked: [] })),
    revokeMyToken: vi.fn(async () => ({ invitations_revoked: [] })),
    regenerateMyToken: vi.fn(async () => ({
      pat: { ...live, id: "p-new", token_last4: "nnnn" },
      token: "iap_fresh_plaintext",
      replaced: "p-live",
      invitations_revoked: [{ id: "i1", email: "lucas@example.org", team_id: "ta", role: "member" }],
    })),
  };
});

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ teams: [{ team_id: "ta", team_name: "Egapro" }] }),
}));

import TokensPanel from "@/views/account/TokensPanel";
import {
  disableMyToken,
  enableMyToken,
  listMyTokens,
  purgeMyToken,
  regenerateMyToken,
} from "@/api/pats";

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

async function rowOf(name: string): Promise<HTMLElement> {
  const row = (await screen.findByText(name, {}, { timeout: 8000 })).closest("tr");
  if (!row) throw new Error(`no table row holds ${name}`);
  return row;
}

function buttonNames(row: HTMLElement): string[] {
  return within(row)
    .queryAllByRole("button")
    .map((b) => b.textContent ?? "");
}

describe("the token lifecycle in the panel", () => {
  it("names each row's status and offers the acts it allows", async () => {
    renderPanel();
    const liveRow = await rowOf("cli");
    expect(within(liveRow).getByText("active")).toBeTruthy();
    expect(buttonNames(liveRow)).toEqual(["Regenerate", "Disable", "Revoke"]);
    const offRow = await rowOf("laptop");
    expect(within(offRow).getByText("disabled")).toBeTruthy();
    expect(buttonNames(offRow)).toEqual(["Regenerate", "Enable", "Revoke"]);
    expect(screen.queryByText("old")).toBeNull();
  });

  it("lists the revoked tokens on request, with Delete as their only act", async () => {
    renderPanel();
    await rowOf("cli");
    expect(vi.mocked(listMyTokens)).toHaveBeenLastCalledWith({ includeRevoked: false });
    fireEvent.click(screen.getByLabelText("Show revoked"));
    const deadRow = await rowOf("old");
    expect(vi.mocked(listMyTokens)).toHaveBeenLastCalledWith({ includeRevoked: true });
    expect(within(deadRow).getByText("revoked")).toBeTruthy();
    expect(buttonNames(deadRow)).toEqual(["Delete"]);
    fireEvent.click(within(deadRow).getByRole("button", { name: "Delete old" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/audit trail stays/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await vi.waitFor(() => expect(vi.mocked(purgeMyToken)).toHaveBeenCalledWith("p-dead"));
  });

  it("disables a live token and enables a disabled one", async () => {
    renderPanel();
    fireEvent.click(within(await rowOf("cli")).getByRole("button", { name: "Disable cli" }));
    await vi.waitFor(() => expect(vi.mocked(disableMyToken)).toHaveBeenCalledWith("p-live"));
    fireEvent.click(within(await rowOf("laptop")).getByRole("button", { name: "Enable laptop" }));
    await vi.waitFor(() => expect(vi.mocked(enableMyToken)).toHaveBeenCalledWith("p-off"));
    expect(vi.mocked(enableMyToken)).not.toHaveBeenCalledWith("p-live");
    expect(vi.mocked(disableMyToken)).not.toHaveBeenCalledWith("p-off");
  });

  it("regenerates: the new plaintext once, and the invitations the old token's revoke ended", async () => {
    renderPanel();
    fireEvent.click(within(await rowOf("cli")).getByRole("button", { name: "Regenerate cli" }));
    const confirm = await screen.findByRole("dialog");
    expect(within(confirm).getByText(/stops working immediately/)).toBeTruthy();
    fireEvent.click(within(confirm).getByRole("button", { name: "Regenerate" }));
    expect(await screen.findByText("iap_fresh_plaintext", {}, { timeout: 8000 })).toBeTruthy();
    expect(vi.mocked(regenerateMyToken)).toHaveBeenCalledWith("p-live");
    expect(screen.getByText("Regenerating cli ended the invitations it sent")).toBeTruthy();
    expect(screen.getByText("lucas@example.org — Egapro, member")).toBeTruthy();
  });

  it("takes one confirmation as one regenerate: the confirm button holds while it runs", async () => {
    let release: () => void = () => undefined;
    vi.mocked(regenerateMyToken).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = () =>
            resolve({
              pat: { ...live, id: "p-new" },
              token: "iap_fresh_plaintext",
              replaced: "p-live",
              invitations_revoked: [],
            });
        }),
    );
    renderPanel();
    fireEvent.click(within(await rowOf("cli")).getByRole("button", { name: "Regenerate cli" }));
    const confirm = await screen.findByRole("dialog");
    const button = within(confirm).getByRole("button", { name: "Regenerate" });
    fireEvent.click(button);
    fireEvent.click(button);
    fireEvent.click(button);
    expect(vi.mocked(regenerateMyToken)).toHaveBeenCalledTimes(1);
    release();
    expect(await screen.findByText("iap_fresh_plaintext", {}, { timeout: 8000 })).toBeTruthy();
    expect(vi.mocked(regenerateMyToken)).toHaveBeenCalledTimes(1);
  });

  it("says the replaced token still stands when the regenerate could not revoke it", async () => {
    vi.mocked(regenerateMyToken).mockResolvedValueOnce({
      pat: { ...live, id: "p-new" },
      token: "iap_fresh_plaintext",
      replaced: "p-live",
      invitations_revoked: [],
      revoke_error: "revoking token p-live failed: it still authenticates",
    });
    renderPanel();
    fireEvent.click(within(await rowOf("cli")).getByRole("button", { name: "Regenerate cli" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Regenerate" }));
    expect(await screen.findByText("iap_fresh_plaintext", {}, { timeout: 8000 })).toBeTruthy();
    expect(screen.getByText("revoking token p-live failed: it still authenticates")).toBeTruthy();
  });
});
