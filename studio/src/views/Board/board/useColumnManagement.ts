// Column-management glue for BoardView: holds the dialog state, wires the
// native /board/states REST calls, and exposes the per-column callbacks
// the Column header menu + reorder drag need. Kept out of index.tsx so the
// board view stays legible. Every mutation calls refresh() afterward — a
// rename/delete also moves issues server-side, so a full board+issues
// re-pull keeps the byState grouping correct.

import { useCallback, useMemo, useState } from "react";

import {
  addState,
  deleteState,
  reorderStates,
  updateState,
  type NativeBoard,
  type NativeIssue,
  type NativeState,
} from "@/api/native";
import { useAsyncAction } from "@/hooks/useAsyncAction";

import { moveInArray } from "../boardShared";

type ColumnDialog =
  | { kind: "none" }
  | { kind: "add" }
  | { kind: "edit"; state: NativeState }
  | { kind: "delete"; state: NativeState };

export interface UseColumnManagementResult {
  dialog: ColumnDialog;
  openAddColumn: () => void;
  closeDialog: () => void;
  busy: boolean;
  error: string | null;
  onEditColumn: (name: string) => void;
  onDeleteColumn: (name: string) => void;
  onMoveColumn: (name: string, dir: "left" | "right") => void;
  onReorderColumn: (dragged: string, target: string) => void;
  submitAdd: (state: NativeState) => void;
  submitEdit: (patch: {
    name?: string;
    display?: string;
    color?: string;
    eligible?: boolean;
    terminal?: boolean;
  }) => void;
  submitDelete: (migrateTo: string | undefined) => void;
  // issueCount(name) → how many of the current issues sit in that column.
  issueCount: (name: string) => number;
}

export function useColumnManagement({
  board,
  issues,
  refresh,
}: {
  board: NativeBoard | null;
  issues: NativeIssue[];
  refresh: () => Promise<void> | void;
}): UseColumnManagementResult {
  const [dialog, setDialog] = useState<ColumnDialog>({ kind: "none" });
  const action = useAsyncAction();

  const counts = useMemo(() => {
    const m = new Map<string, number>();
    for (const iss of issues) m.set(iss.state, (m.get(iss.state) ?? 0) + 1);
    return m;
  }, [issues]);
  const issueCount = useCallback((name: string) => counts.get(name) ?? 0, [counts]);

  const stateByName = useCallback(
    (name: string) => board?.states.find((s) => s.name === name) ?? null,
    [board],
  );

  const closeDialog = useCallback(() => setDialog({ kind: "none" }), []);
  const openAddColumn = useCallback(() => setDialog({ kind: "add" }), []);

  const onEditColumn = useCallback(
    (name: string) => {
      const st = stateByName(name);
      if (st) setDialog({ kind: "edit", state: st });
    },
    [stateByName],
  );

  const onDeleteColumn = useCallback(
    (name: string) => {
      const st = stateByName(name);
      if (st) setDialog({ kind: "delete", state: st });
    },
    [stateByName],
  );

  // run wraps a mutation: refresh on success, close the dialog. Errors
  // surface via action.error (rendered by the dialog).
  const run = useCallback(
    async (op: () => Promise<unknown>, keepOpenOnError = true) => {
      const ok = await action.run(async () => {
        await op();
        await refresh();
        return true;
      });
      if (ok || !keepOpenOnError) closeDialog();
    },
    [action, refresh, closeDialog],
  );

  const reorder = useCallback(
    async (order: string[]) => {
      // Reorder fires from menu/drag without a dialog open — surface
      // failures through action.error but don't toggle dialog state.
      await action.run(async () => {
        await reorderStates(order);
        await refresh();
        return true;
      });
    },
    [action, refresh],
  );

  const onMoveColumn = useCallback(
    (name: string, dir: "left" | "right") => {
      if (!board) return;
      const names = board.states.map((s) => s.name);
      const next = moveInArray(names, name, dir === "left" ? -1 : 1);
      if (next) void reorder(next);
    },
    [board, reorder],
  );

  const onReorderColumn = useCallback(
    (dragged: string, target: string) => {
      if (!board || dragged === target) return;
      const names = board.states.map((s) => s.name).filter((n) => n !== dragged);
      const at = names.indexOf(target);
      if (at < 0) return;
      names.splice(at, 0, dragged);
      void reorder(names);
    },
    [board, reorder],
  );

  const submitAdd = useCallback(
    (state: NativeState) => void run(() => addState(state)),
    [run],
  );

  const submitEdit = useCallback(
    (patch: {
      name?: string;
      display?: string;
      color?: string;
      eligible?: boolean;
      terminal?: boolean;
    }) => {
      if (dialog.kind !== "edit") return;
      void run(() => updateState(dialog.state.name, patch));
    },
    [dialog, run],
  );

  const submitDelete = useCallback(
    (migrateTo: string | undefined) => {
      if (dialog.kind !== "delete") return;
      void run(() => deleteState(dialog.state.name, migrateTo));
    },
    [dialog, run],
  );

  return {
    dialog,
    openAddColumn,
    closeDialog,
    busy: action.busy,
    error: action.error,
    onEditColumn,
    onDeleteColumn,
    onMoveColumn,
    onReorderColumn,
    submitAdd,
    submitEdit,
    submitDelete,
    issueCount,
  };
}
