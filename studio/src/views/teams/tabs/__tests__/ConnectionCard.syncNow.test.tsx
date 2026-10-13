// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import type { ForgeConnection, ForgeIntegration } from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    syncForgeIntegration: vi.fn(),
    updateForgeIntegration: vi.fn(),
    listForgeIntegrationHooks: vi.fn(async () => []),
  };
});

import { syncForgeIntegration, updateForgeIntegration } from "@/api/forgeConnections";
import { ConnectionCard } from "../integrations/ConnectionCard";

const syncMock = vi.mocked(syncForgeIntegration);
const updateMock = vi.mocked(updateForgeIntegration);

const conn = {
  id: "c1",
  provider: "github",
  kind: "pat",
  status: "active",
  created_at: "2026-10-01T00:00:00Z",
} as ForgeConnection;

const integration = {
  id: "i1",
  connection_id: "c1",
  provider: "github",
  repo_full_name: "org/api",
  bot_ids: ["triage"],
  sync_issues_enabled: true,
} as ForgeIntegration;

// onError is the row's ONLY channel to the operator: the tab wires it to the
// banner it renders above the connection list. It carries the CURRENT verdict,
// so a handler with no success path through onChanged opens by clearing it
// with the empty string — hence `messages()` below, which reads the operator's
// errors and ignores those clears.
function renderCard(onError: (m: string) => void) {
  return render(
    <ConnectionCard
      teamID="t1"
      conn={conn}
      integrations={[integration]}
      repoBots={[]}
      canManage
      onChanged={() => {}}
      onError={onError}
      confirm={async () => true}
    />,
  );
}

const syncNowButton = () => screen.getByTitle("Pull the forge's issues onto the board now");

// The error MESSAGES the operator was shown, in order — a cleared channel ("")
// is not one.
const messages = (onError: ReturnType<typeof vi.fn>) =>
  onError.mock.calls.map(([m]) => m as string).filter((m) => m !== "");

afterEach(() => {
  cleanup();
  syncMock.mockReset();
  updateMock.mockReset();
});

// A sync whose read of the forge window was cut short — a page the provider
// refused, or a window the page budget could not exhaust — is refused whole:
// the endpoint answers 502 and holds the high-water mark, and that refusal is
// the ONLY statement anything makes about a window the board is missing. It
// has to reach the operator who asked for the sync.
describe("RepoRow — what a refused Sync now tells the operator", () => {
  it("reports the error of the pass that just failed", async () => {
    const onError = vi.fn();
    syncMock.mockRejectedValueOnce(
      new Error(
        "sync failed: list issues: 20000 entries read in 200 pages and the forge has not " +
          "reported the window exhausted",
      ),
    );
    renderCard(onError);
    fireEvent.click(syncNowButton());

    await waitFor(() => expect(syncMock).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        messages(onError),
        "a refused sync that reports nothing leaves the operator a board quietly missing a window",
      ).toEqual([expect.stringContaining("200 pages")]),
    );

    // And the SECOND failure must speak of itself, not of the first one.
    syncMock.mockRejectedValueOnce(new Error("sync failed: list issues (page 2): 403 rate limited"));
    fireEvent.click(syncNowButton());
    await waitFor(() => expect(syncMock).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(messages(onError)[1], "the second refusal's own reason").toContain("403 rate limited"),
    );
  });

  it("drops the success banner a refused pass does not own", async () => {
    const onError = vi.fn();
    syncMock.mockResolvedValueOnce({ synced: 250, created: 250, updated: 0 });
    renderCard(onError);
    fireEvent.click(syncNowButton());
    await screen.findByText(/Synced 250 issues/);

    syncMock.mockRejectedValueOnce(new Error("sync failed: list issues (page 2): 403 rate limited"));
    fireEvent.click(syncNowButton());
    await waitFor(() => expect(syncMock).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(
        screen.queryByText(/Synced 250 issues/),
        'an earlier pass\'s green banner left on screen over a refusal reads as "the sync worked"',
      ).toBeNull(),
    );
  });

  // Same channel, same hazard: the refused toggle has to name what the server
  // refused, not a generic fallback and not the previous attempt's reason.
  it("reports why the sync toggle was refused", async () => {
    const onError = vi.fn();
    updateMock.mockRejectedValueOnce(new Error("forge integration: sync_issues_enabled: forbidden"));
    renderCard(onError);
    fireEvent.click(screen.getByLabelText("Sync issues to board"));

    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(messages(onError)).toEqual([expect.stringContaining("forbidden")]));
  });

  it("still reports the counts of a sync that succeeded", async () => {
    const onError = vi.fn();
    syncMock.mockResolvedValueOnce({ synced: 3, created: 1, updated: 2 });
    renderCard(onError);
    fireEvent.click(syncNowButton());

    await screen.findByText(/Synced 3 issues · 1 created · 2 updated/);
    expect(messages(onError), "a sync that worked must report no error").toEqual([]);
    // And it did clear the channel on its way in: that empty string is what
    // takes an earlier refusal's banner off the operator's screen.
    expect(onError.mock.calls.map(([m]) => m)).toEqual([""]);
  });
});
