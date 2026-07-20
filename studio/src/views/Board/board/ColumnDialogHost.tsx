// Renders the active column-management dialog (add / edit / delete) from
// the useColumnManagement dialog union. Statement-level narrowing keeps the
// discriminated union clean (it wouldn't narrow inside JSX .map/.filter
// callbacks).

import type { NativeBoard } from "@/api/native";

import {
  AddColumnDialog,
  DeleteColumnDialog,
  EditColumnDialog,
} from "../ColumnDialogs";

import type { UseColumnManagementResult } from "./useColumnManagement";

export function ColumnDialogHost({
  board,
  columns,
}: {
  board: NativeBoard;
  columns: UseColumnManagementResult;
}) {
  const colDialog = columns.dialog;
  if (colDialog.kind === "add") {
    return (
      <AddColumnDialog
        existingNames={board.states.map((s) => s.name)}
        busy={columns.busy}
        error={columns.error}
        onCancel={columns.closeDialog}
        onSubmit={columns.submitAdd}
      />
    );
  }
  if (colDialog.kind === "edit") {
    const st = colDialog.state;
    return (
      <EditColumnDialog
        state={st}
        issueCount={columns.issueCount(st.name)}
        existingNames={board.states.map((s) => s.name).filter((n) => n !== st.name)}
        busy={columns.busy}
        error={columns.error}
        onCancel={columns.closeDialog}
        onSubmit={columns.submitEdit}
      />
    );
  }
  if (colDialog.kind === "delete") {
    const st = colDialog.state;
    return (
      <DeleteColumnDialog
        state={st}
        issueCount={columns.issueCount(st.name)}
        otherStates={board.states.filter((s) => s.name !== st.name)}
        isLast={board.states.length <= 1}
        busy={columns.busy}
        error={columns.error}
        onCancel={columns.closeDialog}
        onSubmit={columns.submitDelete}
      />
    );
  }
  return null;
}
