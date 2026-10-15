// @vitest-environment jsdom
// The merge-gate posture of a repo that is ALREADY integrated lives on its
// integration's launch_vars, and a PATCH's launch_vars is the exact desired
// set: a write that sends `gate_enabled` alone drops every other pin the repo
// carries (its review tier, its gate context) without a word. So this witness
// reads the whole map that leaves the page, and what the page shows for a
// member.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { ForgeIntegration, ForgeTeamRepo } from "@/api/forgeConnections";

import { encodeRepoKey } from "./repoKey";

let canManage = true;
let repoRow: ForgeTeamRepo;
let integrations: ForgeIntegration[] = [];
/** Set to make the integrations read fail, as a down store does. */
let integrationsFailure: string | null = null;

vi.mock("wouter", async () => {
  const actual = await vi.importActual<typeof import("wouter")>("wouter");
  return { ...actual, useParams: () => ({ key: encodeRepoKey("c1::org/api") }) };
});

vi.mock("@/hooks/useActiveRepo", () => ({
  useActiveRepo: () => ({
    activeRepo: repoRow,
    overview: false,
    repos: [repoRow],
    loading: false,
    enabled: true,
    choose: vi.fn(),
    teamID: "t1",
  }),
}));

vi.mock("@/hooks/useCanManageTeam", () => ({
  useCanManageTeam: () => canManage,
}));

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeIntegrations: vi.fn(async () => {
      if (integrationsFailure !== null) throw new Error(integrationsFailure);
      return integrations;
    }),
    updateForgeRepoBots: vi.fn(async () => ({})),
  };
});

vi.mock("@/api/schedules", async () => {
  const actual = await vi.importActual<typeof import("@/api/schedules")>("@/api/schedules");
  return { ...actual, listTeamSchedules: vi.fn(async () => []) };
});

import { updateForgeRepoBots } from "@/api/forgeConnections";
import { useBotsStore } from "@/store/bots";

import RepoDetailView from "./index";

const updateMock = vi.mocked(updateForgeRepoBots);

const row = (over: Partial<ForgeTeamRepo> = {}): ForgeTeamRepo =>
  ({
    connection_id: "c1",
    connection_status: "active",
    provider: "gitlab",
    repo_full_name: "org/api",
    integration_id: "i1",
    bot_ids: ["review-pr"],
    sync_issues_enabled: false,
    gates_merges: true,
    ...over,
  }) as ForgeTeamRepo;

const integration = (launchVars?: Record<string, string>): ForgeIntegration =>
  ({
    id: "i1",
    connection_id: "c1",
    provider: "gitlab",
    repo_full_name: "org/api",
    bot_ids: ["review-pr"],
    events_normalized: ["merge_request"],
    webhook_id: "w1",
    hook_id: "h1",
    launch_vars: launchVars,
    created_at: "2026-10-01T00:00:00Z",
  }) as ForgeIntegration;

const renderPage = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <RepoDetailView />
    </QueryClientProvider>,
  );

beforeEach(() => {
  vi.clearAllMocks();
  canManage = true;
  repoRow = row();
  integrations = [integration({ review_tier: "audit", gate_context: "iterion/revi" })];
  integrationsFailure = null;
  useBotsStore.setState({ bots: [], loading: false, error: null });
});

afterEach(cleanup);

describe("RepoDetail — the Merge gate card", () => {
  it("writes the posture over the repo's WHOLE launch_vars map", async () => {
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    // Nothing pinned yet = the gate is on, which is what the server does.
    expect(select.value).toBe("on");

    fireEvent.change(select, { target: { value: "off" } });
    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(1));
    expect(updateMock.mock.calls[0]).toEqual([
      "t1",
      "i1",
      // No bot set: an absent bot_ids keeps the stored one, while this page's
      // snapshot would unbind a bot bound since it loaded.
      undefined,
      undefined,
      // The review tier and the gate context travel with the change.
      { review_tier: "audit", gate_context: "iterion/revi", gate_enabled: "false" },
    ]);
  });

  it("leaves the stored bot set alone on the review-tier write too", async () => {
    // Same class, same page: a launch-vars write never carries a bot set.
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Review tier");
    fireEvent.change(select, { target: { value: "glance" } });
    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(1));
    expect(updateMock.mock.calls[0]).toEqual([
      "t1",
      "i1",
      undefined,
      undefined,
      { review_tier: "glance", gate_context: "iterion/revi" },
    ]);
  });

  it("turns the gate back on with the value the bots read as truthy", async () => {
    integrations = [integration({ gate_enabled: "false" })];
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    expect(select.value, "an advisory-only repo must not read as gated").toBe("off");

    fireEvent.change(select, { target: { value: "on" } });
    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(1));
    expect(updateMock.mock.calls[0]?.[4]).toEqual({ gate_enabled: "true" });
  });

  it("shows a member the posture without a way to change it", async () => {
    canManage = false;
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    expect(select.disabled, "the server answers 403 — the select must say so").toBe(true);
  });

  it("carries the card on a pinned repo even when the server could not say", async () => {
    // gates_merges is OMITTED when a bot of the row cannot be resolved: that
    // is "unknown", and a repo whose posture IS pinned must still be able to
    // change it.
    repoRow = row({ gates_merges: undefined });
    integrations = [integration({ gate_enabled: "false" })];
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    expect(select.value).toBe("off");
  });

  it("states no posture while the repo's integration cannot be read", async () => {
    // The posture lives on the integration: with that read failed the page
    // holds nothing to state, and "nothing pinned = gated" would be the
    // opposite of the truth for an advisory-only repo.
    integrationsFailure = "API error 500: store down";
    renderPage();

    await screen.findByText(/store down/);
    expect(screen.queryByLabelText("Merge gate")).toBeNull();
  });

  it("shows no card for a repo where nothing gates merges and nothing is pinned", async () => {
    repoRow = row({ gates_merges: false });
    integrations = [integration({ review_tier: "guard" })];
    renderPage();

    // The page itself is up…
    await screen.findByText("Bound bots");
    // …and offers no posture: no bot here posts a merge-gate status.
    expect(screen.queryByLabelText("Merge gate")).toBeNull();
  });

  it("names where the posture lives, in its own card", async () => {
    renderPage();
    await screen.findByLabelText("Merge gate");

    // Scoped to this card: the review-tier card above carries the same
    // sentence, and a caption read from the page as a whole would pass with
    // none of its own.
    const card = screen.getByRole("heading", { name: "Merge gate" }).closest("div");
    expect(card).not.toBeNull();
    expect(within(card as HTMLElement).getByLabelText("Merge gate")).toBeTruthy();
    within(card as HTMLElement).getByText(
      "Pinned on this repo's integration — survives re-provisioning.",
    );
    // And what the posture moves beyond the status itself: provisioning stops
    // forcing review_on_sync for every bot co-enabled on the repo.
    within(card as HTMLElement).getByText(
      "Turning the gate off also stops forcing a re-review on every push, for every bot on this repo.",
    );
  });
});
