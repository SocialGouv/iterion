import { Button } from "@/components/ui/Button";
import { Tooltip } from "@/components/ui";
import type { RunSummary } from "@/api/runs";

import { isCancellable, isDeletable } from "../runStatusActions";

// RunSelectionToolbar is the action bar shown whenever ≥1 run row is
// selected (desktop table only — the mobile cards have no selection).
// Mirrors the Board's SelectionToolbar styling. Each action targets the
// applicable subset of the selection and is disabled when that subset
// is empty; the count suffix surfaces partial applicability.
export function RunSelectionToolbar({
  selectedRuns,
  onCancel,
  onDelete,
  onClear,
}: {
  selectedRuns: RunSummary[];
  onCancel: () => void;
  onDelete: () => void;
  onClear: () => void;
}) {
  const cancellable = selectedRuns.filter((r) => isCancellable(r.status)).length;
  const deletable = selectedRuns.filter((r) => isDeletable(r.status)).length;
  const partial = (n: number) =>
    n > 0 && n < selectedRuns.length ? ` (${n})` : "";
  return (
    <div className="shrink-0 px-3 py-1.5 border-b border-border-default bg-accent-soft hidden sm:flex flex-wrap items-center gap-2 text-xs text-fg-default">
      <span>
        <strong>{selectedRuns.length}</strong> selected
      </span>
      <Tooltip
        content={
          cancellable > 0
            ? "Stop the selected running/queued runs. Checkpoints are saved, so they stay resumable."
            : "Only running or queued runs can be cancelled"
        }
      >
        <Button
          variant="secondary"
          size="sm"
          onClick={onCancel}
          disabled={cancellable === 0}
        >
          Cancel{partial(cancellable)}
        </Button>
      </Tooltip>
      <Tooltip
        content={
          deletable > 0
            ? "Permanently delete the selected terminal runs and all their data"
            : "Only finished, failed, or cancelled runs can be deleted — cancel active runs first"
        }
      >
        <Button
          variant="danger"
          size="sm"
          onClick={onDelete}
          disabled={deletable === 0}
        >
          Delete{partial(deletable)}
        </Button>
      </Tooltip>
      <div className="ml-auto">
        <Button variant="ghost" size="sm" onClick={onClear}>
          clear
        </Button>
      </div>
    </div>
  );
}
