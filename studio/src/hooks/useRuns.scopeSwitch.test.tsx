// @vitest-environment jsdom
import { type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";

import type { AuthResponse } from "@/api/auth";
import type { RunStatus, RunSummary } from "@/api/runs";

// End-to-end wiring of the scope-switch contract: useRuns({keepPrevious})
// mounted under the REAL AuthProvider, driven through a real selectTeam.
// This exercises the removeQueries-on-switch + keepPreviousData interaction
// that the unit-level tests mock away. Two things must hold:
//   1. MID-SWITCH (new fetch pending): keepPreviousData keeps the previous
//      team's rows on screen with refreshing=true — the smooth transition
//      the PR exists for. removeQueries drops the *cache entry*, but the
//      observer keeps its reference, so the rows do NOT blank.
//   2. SETTLED: the list resolves to the new team's rows and never leaves
//      the previous team's runs behind.

function makeDeferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

const listRuns = vi.fn<(opts: unknown) => Promise<RunSummary[]>>();
vi.mock("@/api/runs", () => ({ listRuns: (opts: unknown) => listRuns(opts) }));

vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (selector: (s: unknown) => unknown) =>
    selector({ info: { mode: "cloud" } }),
}));

// Auth API: bootstrap resolves to a two-team identity; switchTeam flips the
// active team id so useRuns re-keys.
const identity = (activeTeamID: string): AuthResponse =>
  ({
    user: { id: "u", email: "e@x.test", status: "active", is_super_admin: true },
    orgs: [
      {
        org_id: "org-a",
        teams: [
          { team_id: "team-a", role: "owner" },
          { team_id: "team-b", role: "owner" },
        ],
      },
    ],
    active_org_id: "org-a",
    active_team_id: activeTeamID,
    active_role: "owner",
  }) as unknown as AuthResponse;

const switchTeam = vi.fn(async (id: string) => identity(id));
vi.mock("@/api/auth", () => ({
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number) {
      super("api");
      this.status = status;
    }
  },
  getMe: vi.fn(async () => identity("team-a")),
  login: vi.fn(),
  logout: vi.fn(),
  register: vi.fn(),
  refresh: vi.fn(),
  switchOrg: vi.fn(),
  switchTeam: (id: string) => switchTeam(id),
}));

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { useRuns } from "./useRuns";

function run(id: string, status: RunStatus = "finished"): RunSummary {
  return { id, status } as RunSummary;
}

beforeEach(() => {
  listRuns.mockReset();
  switchTeam.mockClear();
  // probeAuth (private in AuthContext) hits /server/info via fetch.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(JSON.stringify({ auth_required: true }), { status: 200 }),
    ),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("team switch: keepPreviousData transition (removeQueries + observer)", () => {
  it("holds the previous team's rows (refreshing) mid-switch, then swaps", async () => {
    // team-a resolves immediately; team-b is held open so we can observe
    // the mid-switch window deterministically.
    let currentTeam = "team-a";
    const teamB = makeDeferred<RunSummary[]>();
    listRuns.mockImplementation(async () =>
      currentTeam === "team-a" ? [run("a1")] : teamB.promise,
    );
    switchTeam.mockImplementation(async (id: string) => {
      currentTeam = id;
      return identity(id);
    });

    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: 0 } },
    });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>
        <AuthProvider>{children}</AuthProvider>
      </QueryClientProvider>
    );

    const { result } = renderHook(
      () => ({ auth: useAuth(), runs: useRuns({ keepPrevious: true }) }),
      { wrapper },
    );

    // Wait for bootstrap + first runs fetch (team-a).
    await waitFor(() =>
      expect(result.current.runs.runs.map((r) => r.id)).toEqual(["a1"]),
    );

    // Switch team; team-b's fetch stays pending. Even though removeQueries
    // dropped the old cache entry, keepPreviousData holds the previous rows
    // via the observer, and refreshing flags the in-flight switch — the
    // list must NOT blank. This is the PR's core transition.
    await act(async () => {
      await result.current.auth.selectTeam("team-b");
    });
    expect(result.current.runs.runs.map((r) => r.id)).toEqual(["a1"]);
    expect(result.current.runs.refreshing).toBe(true);
    expect(result.current.runs.loading).toBe(false);

    // Resolve team-b → list swaps and never leaves team-a's run behind.
    teamB.resolve([run("b1")]);
    await waitFor(() =>
      expect(result.current.runs.runs.map((r) => r.id)).toEqual(["b1"]),
    );
    expect(result.current.runs.runs.map((r) => r.id)).not.toContain("a1");
    expect(result.current.runs.refreshing).toBe(false);
  });
});
