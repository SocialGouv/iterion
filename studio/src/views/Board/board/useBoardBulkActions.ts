import { useCallback } from "react";

import {
  deleteIssue,
  patchIssue,
  type NativeBoard,
  type NativeIssue,
  type NativeIssuePatch,
} from "@/api/native";

import type { ConfirmOptions } from "@/hooks/useConfirm";
import type { Toast, ToastAction } from "@/store/ui";

import {
  BULK_DISPATCH_CONFIRM_THRESHOLD,
  BULK_PATCH_CONFIRM_THRESHOLD,
  isDispatchable,
} from "./boardSort";

type AddToast = (
  message: string,
  type: Toast["type"],
  opts?: { action?: ToastAction; persistent?: boolean },
) => void;

type ConfirmFn = (options: ConfirmOptions) => Promise<boolean>;

export interface UseBoardBulkActionsResult {
  onBulkDispatch: () => Promise<void>;
  onBulkMove: (toState: string) => Promise<void>;
  onBulkPriority: (priority: number) => void;
  onBulkAssignee: (assignee: string) => void;
  onBulkToggleLabel: (label: string) => void;
  onBulkDelete: () => Promise<void>;
}

// Owns the bulk-action handlers fired from the SelectionToolbar. All
// callbacks share the same shape: drive a per-issue mutation across
// `selectedIssues`, refresh on success, surface failures via setError,
// and toast the outcome. Confirm-gated where the action is destructive
// or expensive (delete, large-batch dispatch).
export function useBoardBulkActions({
  board,
  selectedIssues,
  dispatchState,
  dispatcherEnabled,
  onDrop,
  refresh,
  setError,
  setSingleSelection,
  addToast,
  confirm,
  setLocation,
}: {
  board: NativeBoard | null;
  selectedIssues: NativeIssue[];
  dispatchState: string;
  // Selects the verb in confirm/toast copy: "Dispatch" when a dispatcher
  // claims from the eligible lane, "Stage" when cards launch their
  // assigned bot on arrival instead.
  dispatcherEnabled: boolean;
  onDrop: (
    issueID: string,
    toState: string,
    opts?: { recordHistory?: boolean },
  ) => Promise<boolean>;
  refresh: () => Promise<void>;
  setError: React.Dispatch<React.SetStateAction<string | null>>;
  setSingleSelection: (id: string | null) => void;
  addToast: AddToast;
  confirm: ConfirmFn;
  setLocation: (to: string, opts?: { replace?: boolean }) => void;
}): UseBoardBulkActionsResult {
  const onBulkDispatch = useCallback(async () => {
    const ids = selectedIssues.filter((i) => isDispatchable(i.state)).map((i) => i.id);
    if (ids.length === 0) return;
    // Each dispatch starts a run (cost). Confirm above a small threshold
    // so a fat-fingered select-all + dispatch doesn't fan out a dozen
    // paid runs — pairs with the per-day spend cap.
    const verb = dispatcherEnabled ? "Dispatch" : "Stage";
    const verbed = dispatcherEnabled ? "Dispatched" : "Staged";
    if (ids.length > BULK_DISPATCH_CONFIRM_THRESHOLD) {
      if (
        !(await confirm({
          title: `${verb} ${ids.length} issues?`,
          message: `This starts ${ids.length} runs at once, each consuming budget against the daily spend cap.`,
          confirmLabel: `${verb} ${ids.length}`,
        }))
      )
        return;
    }
    let queued = 0;
    for (const id of ids) {
      if (await onDrop(id, dispatchState)) queued += 1;
    }
    setSingleSelection(null);
    const plural = (n: number) => (n > 1 ? "s" : "");
    if (queued === ids.length) {
      addToast(`${verbed} ${queued} issue${plural(queued)}`, "success", {
        action: { label: "View runs", onClick: () => setLocation("/runs") },
      });
    } else if (queued > 0) {
      // Partial: some transitions failed (claim conflict, rejected move,
      // tracker error). Surface the gap instead of a misleading all-success
      // toast — the unqueued issues never reached the eligible lane, so
      // nothing will run for them.
      addToast(
        `${verbed} ${queued}/${ids.length} issues — ${ids.length - queued} could not be queued`,
        "warning",
        { action: { label: "View runs", onClick: () => setLocation("/runs") } },
      );
    } else {
      addToast(
        `Could not ${verb.toLowerCase()} ${ids.length} issue${plural(ids.length)}`,
        "error",
      );
    }
  }, [
    selectedIssues,
    dispatchState,
    dispatcherEnabled,
    onDrop,
    setSingleSelection,
    addToast,
    confirm,
    setLocation,
  ]);

  // runBulkPatch applies a per-issue PATCH across the selection, then
  // refreshes. The patch fn returns null to skip an issue (e.g. a label
  // it already has). Shared by the bulk label / priority / assignee ops.
  const runBulkPatch = useCallback(
    async (
      build: (iss: NativeIssue) => NativeIssuePatch | null,
      toastMsg: string,
      // When provided, a selection at/above BULK_PATCH_CONFIRM_THRESHOLD
      // asks for confirmation first (priority/assignee have no undo).
      confirmOpts?: ConfirmOptions,
    ) => {
      const targets = selectedIssues;
      if (targets.length === 0) return;
      if (confirmOpts && targets.length >= BULK_PATCH_CONFIRM_THRESHOLD) {
        if (!(await confirm(confirmOpts))) return;
      }
      try {
        let n = 0;
        for (const iss of targets) {
          const patch = build(iss);
          if (patch) {
            await patchIssue(iss.id, patch);
            n++;
          }
        }
        await refresh();
        if (n > 0) addToast(toastMsg, "success");
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [selectedIssues, refresh, addToast, setError, confirm],
  );

  const onBulkMove = useCallback(
    async (toState: string) => {
      // Capture each issue's origin state so the toast can offer a
      // one-click Undo (bulk move is reversible — no confirm needed).
      const moved = selectedIssues.map((i) => ({ id: i.id, from: i.state }));
      if (moved.length === 0) return;
      for (const m of moved) await onDrop(m.id, toState);
      const display = board?.states.find((s) => s.name === toState)?.display ?? toState;
      addToast(`Moved ${moved.length} to ${display}`, "success", {
        action: {
          label: "Undo",
          onClick: () => {
            void (async () => {
              for (const m of moved) await onDrop(m.id, m.from);
            })();
          },
        },
      });
    },
    [selectedIssues, onDrop, board, addToast],
  );

  const onBulkPriority = useCallback(
    (priority: number) =>
      void runBulkPatch(
        () => ({ priority }),
        `Set priority P${priority} on ${selectedIssues.length} issue${selectedIssues.length > 1 ? "s" : ""}`,
        {
          title: `Set priority P${priority} on ${selectedIssues.length} issues?`,
          message: "Bulk priority changes have no one-click undo.",
          confirmLabel: "Set priority",
        },
      ),
    [runBulkPatch, selectedIssues],
  );

  const onBulkAssignee = useCallback(
    (assignee: string) =>
      void runBulkPatch(
        () => ({ assignee }),
        assignee
          ? `Assigned ${selectedIssues.length} to @${assignee}`
          : `Cleared assignee on ${selectedIssues.length} issue${selectedIssues.length > 1 ? "s" : ""}`,
        {
          title: assignee
            ? `Assign ${selectedIssues.length} issues to @${assignee}?`
            : `Clear assignee on ${selectedIssues.length} issues?`,
          message: "Bulk assignee changes have no one-click undo.",
          confirmLabel: assignee ? "Assign" : "Clear",
        },
      ),
    [runBulkPatch, selectedIssues],
  );

  // Toggle a label across the selection: if every selected issue already
  // has it, remove it from all; otherwise add it to those missing it.
  const onBulkToggleLabel = useCallback(
    (label: string) => {
      const allHave =
        selectedIssues.length > 0 &&
        selectedIssues.every((i) => (i.labels ?? []).includes(label));
      void runBulkPatch(
        (iss) => {
          const cur = iss.labels ?? [];
          const has = cur.includes(label);
          if (allHave) return has ? { labels: cur.filter((l) => l !== label) } : null;
          return has ? null : { labels: [...cur, label] };
        },
        `${allHave ? "Removed" : "Added"} label "${label}" ${allHave ? "from" : "on"} ${selectedIssues.length} issue${selectedIssues.length > 1 ? "s" : ""}`,
      );
    },
    [selectedIssues, runBulkPatch],
  );

  const onBulkDelete = useCallback(async () => {
    const ids = selectedIssues.map((i) => i.id);
    if (ids.length === 0) return;
    if (
      !(await confirm({
        title: `Delete ${ids.length} issue${ids.length > 1 ? "s" : ""}?`,
        message: "This removes them from the board and cannot be undone.",
        confirmLabel: "Delete",
        confirmVariant: "danger",
      }))
    )
      return;
    try {
      for (const id of ids) await deleteIssue(id);
      setSingleSelection(null);
      addToast(`Deleted ${ids.length} issue${ids.length > 1 ? "s" : ""}`, "success");
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [selectedIssues, confirm, setSingleSelection, addToast, refresh, setError]);

  return {
    onBulkDispatch,
    onBulkMove,
    onBulkPriority,
    onBulkAssignee,
    onBulkToggleLabel,
    onBulkDelete,
  };
}
