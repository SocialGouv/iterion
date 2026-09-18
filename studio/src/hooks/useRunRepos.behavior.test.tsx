// @vitest-environment jsdom
import { type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, renderHook, waitFor } from "@testing-library/react";

import type { RunRepo } from "@/api/runs";

const listRunRepos = vi.fn<() => Promise<RunRepo[]>>();
vi.mock("@/api/runs", () => ({ listRunRepos: () => listRunRepos() }));

let activeOrgID: string;
let activeTeamID: string;
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({
    activeOrgID,
    activeTeamID,
    activeTeam: activeTeamID ? { team_id: activeTeamID } : undefined,
  }),
}));

import { useRunRepos } from "./useRunRepos";

function repo(path: string): RunRepo {
  return { project_path: path, count: 1 } as RunRepo;
}

function makeWrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return { wrapper };
}

beforeEach(() => {
  listRunRepos.mockReset();
  activeOrgID = "org-a";
  activeTeamID = "team-a";
});

afterEach(() => cleanup());

describe("useRunRepos team-aware caching", () => {
  it("refetches the distinct-repos set when the team changes", async () => {
    listRunRepos.mockImplementation(async () =>
      activeTeamID === "team-a" ? [repo("org/a")] : [repo("org/b")],
    );
    const { wrapper } = makeWrapper();

    const { result, rerender } = renderHook(() => useRunRepos(true), { wrapper });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.repos.map((r) => r.project_path)).toEqual(["org/a"]);

    activeTeamID = "team-b";
    rerender();
    await waitFor(() =>
      expect(result.current.repos.map((r) => r.project_path)).toEqual(["org/b"]),
    );
    expect(listRunRepos).toHaveBeenCalledTimes(2);
  });

  it("does not fetch when disabled (local mode)", async () => {
    listRunRepos.mockResolvedValue([repo("org/a")]);
    const { wrapper } = makeWrapper();

    const { result } = renderHook(() => useRunRepos(false), { wrapper });
    await Promise.resolve();
    expect(result.current.repos).toEqual([]);
    expect(listRunRepos).not.toHaveBeenCalled();
  });
});
