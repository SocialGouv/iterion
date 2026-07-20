// Issue-modal glue: renders the create and edit <IssueModal> instances for
// the board. The dispatch affordance closes the modal first, then hands the
// issue id back to the orchestrator (which stages/dispatches + toasts).

import type { NativeBoard, NativeIssue } from "@/api/native";

import IssueModal, { type IssueDraft } from "../IssueModal";

import { isDispatchable } from "./boardSort";

export function IssueModalHost({
  board,
  creating,
  editing,
  allAssignees,
  onCreate,
  onSave,
  onCloseCreate,
  onCloseEdit,
  onDelete,
  onDispatch,
}: {
  board: NativeBoard;
  creating: boolean;
  editing: NativeIssue | null;
  allAssignees: string[];
  onCreate: (input: IssueDraft) => Promise<void>;
  onSave: (input: IssueDraft) => Promise<void>;
  onCloseCreate: () => void;
  onCloseEdit: () => void;
  onDelete: (id: string) => void;
  onDispatch: (id: string) => void;
}) {
  return (
    <>
      {creating && (
        <IssueModal
          board={board}
          initial={null}
          onSubmit={onCreate}
          onClose={onCloseCreate}
          allAssignees={allAssignees}
        />
      )}
      {editing && (
        <IssueModal
          board={board}
          initial={editing}
          allAssignees={allAssignees}
          onSubmit={onSave}
          onClose={onCloseEdit}
          onDelete={() => onDelete(editing.id)}
          onDispatch={
            isDispatchable(editing.state)
              ? () => {
                  const id = editing.id;
                  onCloseEdit();
                  onDispatch(id);
                }
              : undefined
          }
        />
      )}
    </>
  );
}
