// @vitest-environment jsdom
import { type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, renderHook, waitFor } from "@testing-library/react";

import type { RunStatus, RunSummary } from "@/api/runs";

// The hook derives its cache key + fetch from three collaborators:
//  - listRuns (the HTTP boundary)
//  - useServerInfoStore (cloud vs local mode)
//  - useAuth (the active team)
// Mock all three so we can assert the team-aware key + keepPreviousData
// + refreshing contract without a live backend.
const listRuns = vi.fn<(opts: unknown) => Promise<RunSummary[]>>();
vi.mock("@/api/runs", () => ({ listRuns: (opts: unknown) => listRuns(opts) }));

let mode: "cloud" | "local";
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (selector: (s: unknown) => unknown) =>
    selector({ info: { mode } }),
}));

let activeOrgID: string;
let activeTeamID: string | undefined;
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({
    activeOrgID,
    activeTeam: activeTeamID ? { team_id: activeTeamID } : undefined,
  }),
}));

import { useRuns } from "./useRuns";

function run(id: string, status: RunStatus = "finished"): RunSummary {
  return { id, status } as RunSummary;
}

// A promise whose resolve is callable from the outside — lets a test hold
// a fetch open to observe the transient refreshing state deterministically.
function makeDeferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

function makeWrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return { client, wrapper };
}

beforeEach(() => {
  listRuns.mockReset();
  mode = "cloud";
  activeOrgID = "org-a";
  activeTeamID = "team-a";
});

afterEach(() => cleanup());

describe("useRuns team-aware caching", () => {
  it("keys the cache by team in cloud mode so switching team refetches", async () => {
    listRuns.mockImplementation(async () =>
      activeTeamID === "team-a" ? [run("a1")] : [run("b1")],
    );
    const { wrapper } = makeWrapper();

    const { result, rerender } = renderHook(() => useRuns(), { wrapper });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.runs.map((r) => r.id)).toEqual(["a1"]);

    // Switch team → new key → a fresh fetch resolves to the new team's runs.
    activeTeamID = "team-b";
    rerender();
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["b1"]));
    expect(listRuns).toHaveBeenCalledTimes(2);
  });

  it("keys by org too, so switching org without a resolvable team refetches", async () => {
    // Two orgs can both resolve to NO active team (team_id undefined). A
    // team-only key would then collide on one cache entry and serve the
    // previous org's runs. Folding the org into the key prevents that.
    activeTeamID = undefined;
    listRuns.mockImplementation(async () =>
      activeOrgID === "org-a" ? [run("a1")] : [run("b1")],
    );
    const { wrapper } = makeWrapper();

    const { result, rerender } = renderHook(() => useRuns(), { wrapper });
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["a1"]));

    activeOrgID = "org-b";
    rerender();
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["b1"]));
    expect(listRuns).toHaveBeenCalledTimes(2);
  });

  it("keeps the previous list on screen while the new scope loads (keepPreviousData)", async () => {
    const deferred = makeDeferred<RunSummary[]>();
    listRuns.mockImplementationOnce(async () => [run("a1")]);
    listRuns.mockImplementationOnce(() => deferred.promise);
    const { wrapper } = makeWrapper();

    const { result, rerender } = renderHook(
      () => useRuns({ keepPrevious: true }),
      { wrapper },
    );
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["a1"]));

    // Switch scope; the second fetch is still pending. The list must NOT
    // blank — it holds the previous data and flags itself refreshing.
    activeTeamID = "team-b";
    rerender();
    await waitFor(() => expect(result.current.refreshing).toBe(true));
    expect(result.current.loading).toBe(false);
    expect(result.current.runs.map((r) => r.id)).toEqual(["a1"]);

    // Resolve the pending fetch → list swaps, refreshing clears.
    deferred.resolve([run("b1")]);
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["b1"]));
    expect(result.current.refreshing).toBe(false);
  });

  it("without keepPrevious, a scope switch does NOT hold prior rows or flag refreshing", async () => {
    // Default (opt-out) consumers — home hub, paused badge, command
    // palette — must not surface another scope's runs during a switch.
    // Their key still changes, but with no keepPreviousData the hook goes
    // to a loading state instead of holding the previous scope's rows.
    const deferred = makeDeferred<RunSummary[]>();
    listRuns.mockImplementationOnce(async () => [run("a1")]);
    listRuns.mockImplementationOnce(() => deferred.promise);
    const { wrapper } = makeWrapper();

    const { result, rerender } = renderHook(() => useRuns(), { wrapper });
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["a1"]));

    activeTeamID = "team-b";
    rerender();
    // No placeholder: the previous team's rows are not held, and
    // refreshing never trips (it is derived from isPlaceholderData).
    await waitFor(() => expect(result.current.loading).toBe(true));
    expect(result.current.refreshing).toBe(false);
    expect(result.current.runs).toEqual([]);

    deferred.resolve([run("b1")]);
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["b1"]));
  });

  it("does not scope by team in local mode (single-tenant key stays stable)", async () => {
    mode = "local";
    activeTeamID = undefined;
    listRuns.mockImplementation(async () => [run("local-1")]);
    const { wrapper } = makeWrapper();

    const { result } = renderHook(() => useRuns(), { wrapper });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.runs.map((r) => r.id)).toEqual(["local-1"]);
    expect(listRuns).toHaveBeenCalledTimes(1);
  });

  it("does NOT flag refreshing on a same-key background refetch (poll tick)", async () => {
    // The refreshing flag drives a dim overlay. It must fire ONLY on a
    // scope switch (key change held by keepPreviousData), never on an
    // ordinary same-key poll — otherwise the overlay blinks every few
    // seconds while runs are active. This locks that contract.
    const deferred = makeDeferred<RunSummary[]>();
    listRuns.mockImplementationOnce(async () => [run("a1")]);
    listRuns.mockImplementationOnce(() => deferred.promise);
    const { client, wrapper } = makeWrapper();

    const { result } = renderHook(
      () => useRuns({ keepPrevious: true }),
      { wrapper },
    );
    expect(result.current.loading).toBe(true);
    await waitFor(() => expect(result.current.loading).toBe(false));

    // Same-key refetch held open by the pending promise: isFetching is
    // true, but the data is NOT placeholder (same key), so refreshing
    // stays false and loading stays false.
    void client.refetchQueries({ queryKey: ["runs"] });
    await waitFor(() => expect(result.current.runs).toBeDefined());
    expect(result.current.refreshing).toBe(false);
    expect(result.current.loading).toBe(false);

    deferred.resolve([run("a1")]);
    await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual(["a1"]));
  });
});
