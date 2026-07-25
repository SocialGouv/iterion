// Issue-modal glue for the board: the create/edit modal state plus the
// create / save / delete handlers behind <IssueModalHost>. Delete also
// prunes the removed id from the selection so the toolbar count stays
// truthful.

import { useCallback, useState } from "react";

import {
  createIssue,
  deleteIssue,
  patchIssue,
  type NativeIssue,
} from "@/api/native";
import type { ConfirmOptions } from "@/hooks/useConfirm";

import type { IssueDraft } from "../IssueModal";

export interface UseIssueActionsResult {
  creating: boolean;
  setCreating: (v: boolean) => void;
  editing: NativeIssue | null;
  setEditing: (iss: NativeIssue | null) => void;
  onCreate: (input: IssueDraft) => Promise<void>;
  onSave: (input: IssueDraft) => Promise<void>;
  onDelete: (id: string) => Promise<void>;
}

export function useIssueActions({
  refresh,
  setError,
  confirm,
  setSelectedIds,
  setAnchorId,
}: {
  refresh: () => Promise<void>;
  setError: React.Dispatch<React.SetStateAction<string | null>>;
  confirm: (options: ConfirmOptions) => Promise<boolean>;
  setSelectedIds: React.Dispatch<React.SetStateAction<Set<string>>>;
  setAnchorId: React.Dispatch<React.SetStateAction<string | null>>;
}): UseIssueActionsResult {
  const [editing, setEditing] = useState<NativeIssue | null>(null);
  const [creating, setCreating] = useState(false);

  const onCreate = useCallback(
    async (input: IssueDraft) => {
      try {
        await createIssue({
          title: input.title ?? "",
          body: input.body,
          state: input.state,
          labels: input.labels,
          priority: input.priority,
          assignee: input.assignee,
          fields: input.fields,
          bot: input.bot,
          bot_args: input.bot_args,
          external: input.external,
        });
        setCreating(false);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [refresh, setError],
  );

  const onSave = useCallback(
    async (input: IssueDraft) => {
      if (!editing) return;
      try {
        await patchIssue(editing.id, {
          title: input.title,
          body: input.body,
          labels: input.labels,
          priority: input.priority,
          // assignee/bot/bot_args all default to a cleared value ("" / "" /
          // {}) when the operator empties the field, so the corresponding
          // Patch pointer is SET and the server actually clears a
          // previously-stored value. The modal emits `undefined` for an
          // empty field; without the `?? ""` the key is JSON-dropped, the
          // server reads a nil pointer as "unchanged", and the stale value
          // silently persists. For `assignee` that also kept routing the
          // issue to the wrong per-assignee workflow (assignee selects the
          // bot), so clearing it has to reach the store.
          assignee: input.assignee ?? "",
          fields: input.fields,
          bot: input.bot ?? "",
          bot_args: input.bot_args ?? {},
          external: input.external,
        });
        setEditing(null);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [editing, refresh, setError],
  );

  const onDelete = useCallback(
    async (id: string) => {
      if (
        !(await confirm({
          title: "Delete this issue?",
          message: "This removes it from the board and cannot be undone.",
          confirmLabel: "Delete",
          confirmVariant: "danger",
        }))
      )
        return;
      try {
        await deleteIssue(id);
        setEditing(null);
        setSelectedIds((cur) => {
          if (!cur.has(id)) return cur;
          const next = new Set(cur);
          next.delete(id);
          return next;
        });
        setAnchorId((cur) => (cur === id ? null : cur));
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [confirm, refresh, setError, setSelectedIds, setAnchorId],
  );

  return { creating, setCreating, editing, setEditing, onCreate, onSave, onDelete };
}
