// @vitest-environment jsdom
// The provisioning endpoint refuses two launch_vars writes an operator CAN
// fix, and names the fix (#2173): a request that would drop pins it did not
// mention (400 launch_vars_drop), and a repo whose two settings stores
// disagree (500 provision_diverged / provision_diverged_retry). Until this
// witness the studio showed a bare failure for all three and the operator
// dropped to curl. So the assertions are: the pins at stake are NAMED, the
// gesture offered is the one the refusal's own code calls for, and clicking it
// re-sends the write that was refused.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { apiErrorFrom } from "@/api/client";
import type { ForgeIntegration, ForgeTeamRepo } from "@/api/forgeConnections";

import { encodeRepoKey } from "./repoKey";

let repoRow: ForgeTeamRepo;
let integrations: ForgeIntegration[] = [];

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

vi.mock("@/hooks/useCanManageTeam", () => ({ useCanManageTeam: () => true }));

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeIntegrations: vi.fn(async () => integrations),
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

const row = (): ForgeTeamRepo =>
  ({
    connection_id: "c1",
    connection_status: "active",
    provider: "gitlab",
    repo_full_name: "org/api",
    integration_id: "i1",
    bot_ids: ["review-pr"],
    sync_issues_enabled: false,
    gates_merges: true,
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

// The EXACT envelope pkg/server writes for the drop refusal
// (forge_provisioning_routes.go: a 400 keeps its whole payload, so the keys
// travel as a list read from the typed cause, never parsed out of the prose).
const dropRefusal = () =>
  apiErrorFrom(400, {
    message:
      "forge: launch_vars would drop pinned key(s) [gate_context review_tier] — echo them in launch_vars to keep them, or set launch_vars_replace to replace the whole map",
    errorCode: "launch_vars_drop",
    body: {
      error_code: "launch_vars_drop",
      launch_vars_keys: ["gate_context", "review_tier"],
    },
  });

// And for a divergence: a 500's payload is floored to its code + retryable
// (internal/httpx.WriteErrorJSON), which is exactly why the UI must branch on
// the code and name the pins it holds itself.
const divergedRefusal = (retryable: boolean) =>
  apiErrorFrom(500, {
    message: "internal server error",
    errorCode: retryable ? "provision_diverged_retry" : "provision_diverged",
    body: {
      error_code: retryable ? "provision_diverged_retry" : "provision_diverged",
      retryable,
    },
  });

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
  repoRow = row();
  integrations = [integration({ review_tier: "audit", gate_context: "iterion/revi" })];
  useBotsStore.setState({ bots: [], loading: false, error: null });
});

afterEach(cleanup);

describe("RepoDetail — a launch_vars refusal offers its remedy", () => {
  it("names the dropped pins and echoes them on the echo gesture", async () => {
    updateMock.mockRejectedValueOnce(dropRefusal());
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    fireEvent.change(select, { target: { value: "off" } });

    // The keys the server NAMED, not a generic failure.
    const panel = await screen.findByText(/would drop pins this repo already holds/i);
    expect(panel).toBeTruthy();
    await screen.findByText(/^gate_context = iterion\/revi$/);
    await screen.findByText(/^review_tier = audit$/);

    fireEvent.click(screen.getByRole("button", { name: "Echo the pins" }));
    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(2));
    // The re-send carries the named pins AND the change that was refused.
    expect(updateMock.mock.calls[1]).toEqual([
      "t1",
      "i1",
      undefined,
      undefined,
      { review_tier: "audit", gate_context: "iterion/revi", gate_enabled: "false" },
    ]);
  });

  it("re-sends with launch_vars_replace on the replace gesture", async () => {
    updateMock.mockRejectedValueOnce(dropRefusal());
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    fireEvent.change(select, { target: { value: "off" } });
    await screen.findByText(/would drop pins this repo already holds/i);

    fireEvent.click(screen.getByRole("button", { name: "Replace the whole map" }));
    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(2));
    expect(updateMock.mock.calls[1]).toEqual([
      "t1",
      "i1",
      undefined,
      undefined,
      { review_tier: "audit", gate_context: "iterion/revi", gate_enabled: "false" },
      undefined,
      true,
    ]);
  });

  it("offers a plain retry — and no restatement — for a retry-convergent divergence", async () => {
    updateMock.mockRejectedValueOnce(divergedRefusal(true));
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    fireEvent.change(select, { target: { value: "off" } });

    await screen.findByText(/re-run the same request/i);
    // The gesture an operator must NOT be asked to make here: provisioning
    // converges on its own, so there is nothing to echo or replace.
    expect(screen.queryByRole("button", { name: "Echo the pins" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Replace the whole map" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(2));
    expect(updateMock.mock.calls[1]).toEqual(updateMock.mock.calls[0]);
  });

  it("asks for the explicit gesture on a deterministic divergence", async () => {
    updateMock.mockRejectedValueOnce(divergedRefusal(false));
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    fireEvent.change(select, { target: { value: "off" } });

    await screen.findByText(/disagree between its two stores/i);
    // The 500 carried no key list, so the pins this page holds are the ones
    // named — anything else would be the page inventing the server's answer.
    await screen.findByText(/^gate_context = iterion\/revi$/);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Replace the whole map" }));
    await waitFor(() => expect(updateMock).toHaveBeenCalledTimes(2));
    expect(updateMock.mock.calls[1]?.[6]).toBe(true);
  });

  it("still shows the generic failure for an unnamed failure", async () => {
    updateMock.mockRejectedValueOnce(new Error("store unavailable"));
    renderPage();

    const select = await screen.findByLabelText<HTMLSelectElement>("Merge gate");
    fireEvent.change(select, { target: { value: "off" } });

    await screen.findByText(/store unavailable/);
    expect(screen.queryByRole("button", { name: "Replace the whole map" })).toBeNull();
  });
});
