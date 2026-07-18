import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { cancelRun, deleteRun, resumeRun, type RunSummary } from "@/api/runs";
import type { ConfirmOptions } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import type { Toast, ToastAction } from "@/store/ui";

import { isCancellable, isDeletable } from "../runStatusActions";

type AddToast = (
  message: string,
  type: Toast["type"],
  opts?: { action?: ToastAction; persistent?: boolean },
) => void;

type ConfirmFn = (options: ConfirmOptions) => Promise<boolean>;

export interface UseRunListActionsResult {
  // Inline per-row quick action: resume from the last checkpoint (no
  // force — the ResumeDialog in the run console covers that case).
  onResume: (id: string) => void;
  // Ids with a resume request in flight, to guard double-clicks.
  resumingIds: ReadonlySet<string>;
  onBulkCancel: () => Promise<void>;
  onBulkDelete: () => Promise<void>;
}

const plural = (n: number) => (n > 1 ? "s" : "");

// Owns the run-list mutations (inline resume + bulk cancel/delete),
// mirroring the Board's useBoardBulkActions shape: drive the per-run
// endpoint across the selection, toast the aggregate outcome, then
// invalidate the runs query so the polled list refreshes immediately.
export function useRunListActions({
  selectedRuns,
  clearSelection,
  addToast,
  confirm,
  onOpenRun,
}: {
  selectedRuns: RunSummary[];
  clearSelection: () => void;
  addToast: AddToast;
  confirm: ConfirmFn;
  onOpenRun: (id: string) => void;
}): UseRunListActionsResult {
  const queryClient = useQueryClient();
  const refresh = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["runs"] }),
    [queryClient],
  );

  const [resumingIds, setResumingIds] = useState<ReadonlySet<string>>(
    () => new Set(),
  );

  const onResume = useCallback(
    (id: string) => {
      setResumingIds((prev) => new Set(prev).add(id));
      void (async () => {
        try {
          await resumeRun(id);
          addToast("Resume requested", "info", {
            action: { label: "Open run", onClick: () => onOpenRun(id) },
          });
          await refresh();
        } catch (e) {
          // A hash-mismatch (workflow file changed since launch) needs
          // --force, which only the run console's Resume dialog offers.
          addToast(
            `Resume failed: ${errorMessage(e)} — open the run to resume with force`,
            "error",
          );
        } finally {
          setResumingIds((prev) => {
            const next = new Set(prev);
            next.delete(id);
            return next;
          });
        }
      })();
    },
    [addToast, onOpenRun, refresh],
  );

  const onBulkCancel = useCallback(async () => {
    const ids = selectedRuns
      .filter((r) => isCancellable(r.status))
      .map((r) => r.id);
    if (ids.length === 0) return;
    const results = await Promise.allSettled(ids.map((id) => cancelRun(id)));
    const ok = results.filter((r) => r.status === "fulfilled").length;
    const failed = ids.length - ok;
    if (failed === 0) {
      addToast(`Cancelled ${ok} run${plural(ok)}`, "success");
    } else if (ok > 0) {
      addToast(`Cancelled ${ok}/${ids.length} runs — ${failed} failed`, "warning");
    } else {
      addToast(`Could not cancel ${ids.length} run${plural(ids.length)}`, "error");
    }
    clearSelection();
    await refresh();
  }, [selectedRuns, addToast, clearSelection, refresh]);

  const onBulkDelete = useCallback(async () => {
    const ids = selectedRuns
      .filter((r) => isDeletable(r.status))
      .map((r) => r.id);
    if (ids.length === 0) return;
    const ok0 = await confirm({
      title: `Delete ${ids.length} run${plural(ids.length)}?`,
      message:
        "This permanently removes the selected runs and all their data — events, artifacts, attachments. This cannot be undone.",
      confirmLabel: `Delete ${ids.length}`,
      confirmVariant: "danger",
    });
    if (!ok0) return;
    const results = await Promise.allSettled(ids.map((id) => deleteRun(id)));
    const ok = results.filter((r) => r.status === "fulfilled").length;
    const failed = ids.length - ok;
    if (failed === 0) {
      addToast(`Deleted ${ok} run${plural(ok)}`, "success");
    } else if (ok > 0) {
      addToast(`Deleted ${ok}/${ids.length} runs — ${failed} failed`, "warning");
    } else {
      addToast(`Could not delete ${ids.length} run${plural(ids.length)}`, "error");
    }
    clearSelection();
    await refresh();
  }, [selectedRuns, confirm, addToast, clearSelection, refresh]);

  return { onResume, resumingIds, onBulkCancel, onBulkDelete };
}
