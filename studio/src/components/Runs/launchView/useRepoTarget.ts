// Extracted from LaunchView.tsx to keep that file focused.
// useRepoTarget owns the "Target repository" section state — cloud-only.
// When the bot's manifest declares a `repo:` block (mode != "none"), the
// operator picks attach/create/skip before launch. State is owned here so
// the create-then-launch flow can stage a createForgeRepo() call and pipe
// its output into createRun.

import { useEffect, useMemo, useState } from "react";

import type { RepoRequirement } from "@/api/bots";
import type { ServerInfo } from "@/api/types";
import { useActiveRepo } from "@/hooks/useActiveRepo";

import {
  findAttachedRepo,
  initialRepoTargetState,
  isRepoTargetValid,
  type RepoTargetState,
} from "./RepoTargetSection";
import type { useBotPresets } from "./useBotPresets";

export type UseRepoTargetResult = ReturnType<typeof useRepoTarget>;

export function useRepoTarget(
  bot: ReturnType<typeof useBotPresets>["bot"],
  serverInfo: ServerInfo | null,
) {
  const {
    activeRepo,
    repos: teamRepos,
    teamID,
    enabled: repoContextEnabled,
    loading: repoContextLoading,
  } = useActiveRepo();
  const [repoTargetState, setRepoTargetState] = useState<RepoTargetState | null>(null);
  const [repoTargetCreateError, setRepoTargetCreateError] = useState<string | null>(null);

  // Show the "Target repository" section when: the bot declares a repo need
  // (mode !== "none"), we're in cloud mode with a team context, and the
  // useActiveRepo hook is wired (repos + activeRepo available).
  //
  // In cloud, a bot WITHOUT a manifest `repo:` block still gets an
  // optional attach-only section: a manual cloud launch otherwise runs
  // in the bare runner pod, which is almost never what a code bot wants
  // — the repo target IS the cloud workspace. `mode: "none"` stays an
  // explicit opt-out.
  const repoRequirement = useMemo<RepoRequirement | null>(() => {
    if (bot?.repo) return bot.repo.mode !== "none" ? bot.repo : null;
    if (bot && serverInfo?.mode === "cloud") return { mode: "optional" };
    return null;
  }, [bot, serverInfo?.mode]);
  const showRepoTarget =
    !!repoRequirement && serverInfo?.mode === "cloud" && repoContextEnabled;

  // Initialise / re-initialise the repo-target state when the bot changes.
  // Deliberately keyed on the bot's rel_path + the useActiveRepo loading
  // flag so operator edits aren't clobbered by every useActiveRepo refetch,
  // but the initial pick doesn't miss "Use <active repo>" just because the
  // repos query hadn't landed yet. The activeRepo / teamRepos snapshot is
  // captured via closure at the moment the effect fires.
  const botKey = bot?.rel_path ?? "";
  useEffect(() => {
    if (!repoRequirement) {
      setRepoTargetState(null);
      return;
    }
    if (repoContextLoading) return;
    setRepoTargetState(initialRepoTargetState(repoRequirement, activeRepo, teamRepos));
    setRepoTargetCreateError(null);
    // activeRepo / teamRepos intentionally excluded — we seed from the
    // current snapshot then let the operator drive further changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [botKey, repoRequirement?.mode, repoRequirement?.allow_create, repoContextLoading]);

  const repoTargetValid =
    !repoRequirement ||
    !showRepoTarget ||
    (repoTargetState !== null &&
      isRepoTargetValid(repoRequirement, repoTargetState, activeRepo, teamRepos));
  const missingRepoTarget =
    showRepoTarget && repoRequirement?.mode === "required" && !repoTargetValid;

  // Forge provider of the selected repo target, when knowable — drives
  // PR/MR wording in the worktree-finalization copy. Null (neutral copy)
  // when no repo target is in play or the choice doesn't pin a provider.
  const selectedRepoProvider = useMemo(() => {
    if (!showRepoTarget || !repoTargetState) return null;
    switch (repoTargetState.mode) {
      case "active":
        return activeRepo?.provider ?? null;
      case "attach":
        return findAttachedRepo(teamRepos, repoTargetState.attachKey)?.provider ?? null;
      case "create":
        return (
          teamRepos.find(
            (r) => r.connection_id === repoTargetState.createConnectionID,
          )?.provider ?? null
        );
      case "none":
        return null;
    }
  }, [showRepoTarget, repoTargetState, activeRepo, teamRepos]);

  return {
    activeRepo,
    teamRepos,
    teamID,
    repoRequirement,
    showRepoTarget,
    repoTargetState,
    setRepoTargetState,
    repoTargetCreateError,
    setRepoTargetCreateError,
    missingRepoTarget,
    selectedRepoProvider,
  };
}
