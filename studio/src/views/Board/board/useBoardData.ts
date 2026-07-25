import { useCallback, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import {
  getBoard,
  listIssues,
  type NativeBoard,
  type NativeIssue,
} from "@/api/native";
import { errorMessage } from "@/lib/errorHints";

// Board + issues travel as one cache entry so both land in the same
// render — the columns never see a fresh board against stale issues.
interface BoardData {
  board: NativeBoard;
  issues: NativeIssue[];
}

const BOARD_DATA_KEY = ["board-data"] as const;

// Stable empty fallback so the undefined→loaded transition doesn't hand
// downstream memos a fresh [] reference each render.
const EMPTY_ISSUES: NativeIssue[] = [];

export interface UseBoardDataResult {
  board: NativeBoard | null;
  issues: NativeIssue[];
  setIssues: React.Dispatch<React.SetStateAction<NativeIssue[]>>;
  loading: boolean;
  error: string | null;
  setError: React.Dispatch<React.SetStateAction<string | null>>;
  refresh: () => Promise<void>;
}

// Owns the board + issues fetch. Exposes a refresh() imperative so
// mutating callers (create / save / delete / bulk ops) can re-pull
// after their writes. `setIssues` and `setError` are surfaced so
// optimistic-update / failure paths (onDrop, polls) can patch state
// without going through the round-trip.
export function useBoardData(): UseBoardDataResult {
  const queryClient = useQueryClient();
  const query = useQuery<BoardData>({
    queryKey: BOARD_DATA_KEY,
    queryFn: async () => {
      const [b, i] = await Promise.all([getBoard(), listIssues()]);
      return { board: b, issues: i ?? [] };
    },
  });
  const { refetch } = query;

  // Action-level errors (drag-drop revert, bulk ops, modal saves)
  // overlay the fetch error; refresh() clears them before re-pulling.
  const [actionError, setActionError] = useState<string | null>(null);

  // Patches the cached issues in place (optimistic drag-drop moves,
  // dispatcher-poll deltas) without a round-trip. Before the first
  // load there is nothing to patch — the in-flight initial fetch
  // supersedes such a write either way.
  const setIssues = useCallback<React.Dispatch<React.SetStateAction<NativeIssue[]>>>(
    (action) => {
      queryClient.setQueryData<BoardData>(BOARD_DATA_KEY, (old) => {
        if (!old) return old;
        const issues = typeof action === "function" ? action(old.issues) : action;
        return { ...old, issues };
      });
    },
    [queryClient],
  );

  const refresh = useCallback(async () => {
    setActionError(null);
    await refetch();
  }, [refetch]);

  return {
    board: query.data?.board ?? null,
    issues: query.data?.issues ?? EMPTY_ISSUES,
    setIssues,
    loading: query.isLoading,
    error: actionError ?? (query.error ? errorMessage(query.error) : null),
    setError: setActionError,
    refresh,
  };
}
