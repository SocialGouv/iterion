// @vitest-environment jsdom
//
// The "Target repository" choice is one bot's (rva round 2, S3a). It is
// re-initialised when the bot changes, and the bot is told by its NAME: a
// stored bot — a team's own, a platform override — has no workspace path, so
// two of them share an empty one, and a key on the path would hand the first
// bot's choice to the second.
import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

vi.mock("@/hooks/useActiveRepo", () => ({
  useActiveRepo: () => ({
    activeRepo: null,
    overview: false,
    repos: [],
    loading: false,
    enabled: true,
    choose: () => {},
    teamID: "team-1",
  }),
}));

import type { BotEntryWithSchema } from "@/api/bots";
import type { ServerInfo } from "@/api/types";

import { initialRepoTargetState } from "./RepoTargetSection";
import { useRepoTarget } from "./useRepoTarget";

const cloud = { mode: "cloud" } as ServerInfo;
const stored = (name: string): BotEntryWithSchema => ({
  name,
  path: "",
  origin: "tenant",
  is_bundle: true,
  repo: { mode: "optional" },
});

describe("useRepoTarget", () => {
  it("re-initialises the choice when the form moves from one stored bot to another", () => {
    const { result, rerender } = renderHook(({ bot }) => useRepoTarget(bot, cloud), {
      initialProps: { bot: stored("alpha") },
    });
    const initial = initialRepoTargetState({ mode: "optional" }, null, []);
    expect(result.current.repoTargetState).toEqual(initial);

    // The operator's choice for alpha.
    const chosen = { ...initial, mode: "create" as const, createName: "alpha-repo" };
    act(() => result.current.setRepoTargetState(chosen));
    expect(result.current.repoTargetState).toEqual(chosen);

    rerender({ bot: stored("beta") });
    expect(result.current.repoTargetState).toEqual(initial);
  });

  it("keeps the choice while the same bot is handed again", () => {
    const { result, rerender } = renderHook(({ bot }) => useRepoTarget(bot, cloud), {
      initialProps: { bot: stored("alpha") },
    });
    const chosen = {
      ...initialRepoTargetState({ mode: "optional" }, null, []),
      mode: "create" as const,
      createName: "alpha-repo",
    };
    act(() => result.current.setRepoTargetState(chosen));
    // Another object for the same bot, as a catalog refetch hands one.
    rerender({ bot: stored("alpha") });
    expect(result.current.repoTargetState).toEqual(chosen);
  });
});
