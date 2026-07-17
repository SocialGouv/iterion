import { create } from "zustand";

import { forgeTeamRepoKey, type ForgeTeamRepo } from "@/api/forgeConnections";
import { readStringFlag, writeStringFlag } from "@/lib/localStorageFlag";

// Active-repo CONTEXT (repo-first): the studio scopes most views and
// pre-fills repo-targeting actions on one concrete connected repo. The
// selection is a UI filter, NOT an authz boundary — authorization stays
// the team. Persisted per (user, team) in localStorage. "All repos" is
// the explicit overview mode (aggregate views; actions that need a repo
// ask inline); with zero repos connected the switcher shows its
// connect CTA instead.

const ALL_SENTINEL = "ALL";

export type RepoSelection =
  | { kind: "none" } // not hydrated yet, or nothing connected
  | { kind: "all" } // explicit overview
  | { kind: "repo"; key: string };

interface ActiveRepoState {
  storageKey: string | null;
  selection: RepoSelection;
  hydrate: (userID: string, teamID: string) => void;
  /** Pick a repo key, or null for the "All repos" overview. */
  choose: (key: string | null) => void;
  /** Reconcile the stored choice against the fetched list — repo-first:
   *  no valid stored choice ⇒ auto-select the first connected repo. */
  reconcile: (repos: ForgeTeamRepo[]) => void;
  reset: () => void;
}

export const useActiveRepoStore = create<ActiveRepoState>((set, get) => ({
  storageKey: null,
  selection: { kind: "none" },
  hydrate: (userID, teamID) => {
    const storageKey = `iterion.activeRepo.${userID}.${teamID}`;
    if (get().storageKey === storageKey) return;
    const raw = readStringFlag(storageKey, "");
    set({
      storageKey,
      selection:
        raw === ALL_SENTINEL
          ? { kind: "all" }
          : raw
            ? { kind: "repo", key: raw }
            : { kind: "none" },
    });
  },
  choose: (key) => {
    const { storageKey } = get();
    if (storageKey) writeStringFlag(storageKey, key ?? ALL_SENTINEL);
    set({ selection: key === null ? { kind: "all" } : { kind: "repo", key } });
  },
  reconcile: (repos) => {
    const { selection, storageKey } = get();
    if (selection.kind === "all") return;
    if (selection.kind === "repo" && repos.some((r) => forgeTeamRepoKey(r) === selection.key)) {
      return;
    }
    // Stored repo vanished (disconnected) or nothing stored yet.
    const first = repos[0];
    if (first === undefined) {
      if (selection.kind === "repo") set({ selection: { kind: "none" } });
      return;
    }
    const key = forgeTeamRepoKey(first);
    if (storageKey) writeStringFlag(storageKey, key);
    set({ selection: { kind: "repo", key } });
  },
  reset: () => set({ storageKey: null, selection: { kind: "none" } }),
}));
