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
// that the unit-level tests mock away — the concern being whether a team
// switch leaks the previous tenant's rows.

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

describe("team switch: run scope isolation (removeQueries + keepPreviousData)", () => {
  it("does not leak the previous team's rows after a switch", async () => {
    // Rows resolve by the team currently active in the session.
    let currentTeam = "team-a";
    listRuns.mockImplementation(async () =>
      currentTeam === "team-a" ? [run("a1")] : [run("b1")],
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

    // Switch team. removeQueries drops the previous scope's cache, so the
    // team-a rows must NOT remain on screen once the switch settles: the
    // list resolves to team-b's rows and never exposes a1 as team-b data.
    await act(async () => {
      await result.current.auth.selectTeam("team-b");
    });
    await waitFor(() =>
      expect(result.current.runs.runs.map((r) => r.id)).toEqual(["b1"]),
    );
    // Never leaks the old tenant's run into the new scope.
    expect(result.current.runs.runs.map((r) => r.id)).not.toContain("a1");
  });
});
