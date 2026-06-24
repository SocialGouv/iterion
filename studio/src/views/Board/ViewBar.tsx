// ViewBar — saved-view picker for the board. Sits under the filter bar:
// pick a saved view to restore its search/labels/assignee/sort/grouping,
// save the current filter combo as a named view, or delete the active
// one. Views are persisted in board.json (shared across operators).

import { useState } from "react";

import type { NativeView } from "@/api/native";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

export function ViewBar({
  views,
  activeView,
  onApply,
  onSave,
  onDelete,
  busy,
  error,
}: {
  views: NativeView[];
  activeView: string;
  onApply: (v: NativeView | null) => void;
  onSave: (name: string) => void;
  onDelete: (name: string) => void;
  busy: boolean;
  error: string | null;
}) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [name, setName] = useState("");

  if (views.length === 0 && !dialogOpen) {
    // Nothing saved yet — offer only the "Save current view" affordance.
    return (
      <div className="px-3 py-1.5 border-b border-border-default bg-surface-0 flex items-center gap-2 text-xs">
        <span className="text-fg-muted">Views</span>
        <Button variant="ghost" size="sm" onClick={() => setDialogOpen(true)}>
          + Save current view
        </Button>
        {error && <span className="text-danger-fg">{error}</span>}
        {dialogOpen && (
          <SaveDialog
            name={name}
            existing={views.map((v) => v.name)}
            busy={busy}
            onChange={setName}
            onCancel={() => setDialogOpen(false)}
            onSubmit={() => {
              onSave(name.trim());
              setDialogOpen(false);
              setName("");
            }}
          />
        )}
      </div>
    );
  }

  return (
    <div className="px-3 py-1.5 border-b border-border-default bg-surface-0 flex items-center gap-2 text-xs">
      <label htmlFor="board-view-select" className="text-fg-muted">
        Views
      </label>
      <Select
        id="board-view-select"
        value={activeView}
        onChange={(e) => {
          const v = views.find((x) => x.name === e.target.value) ?? null;
          onApply(v);
        }}
        aria-label="Saved views"
      >
        <option value="">Custom…</option>
        {views.map((v) => (
          <option key={v.name} value={v.name}>
            {v.name}
          </option>
        ))}
      </Select>
      <Button variant="ghost" size="sm" onClick={() => setDialogOpen(true)}>
        Save view…
      </Button>
      {activeView && (
        <Button
          variant="ghost"
          size="sm"
          className="text-danger-fg hover:text-danger"
          onClick={() => onDelete(activeView)}
        >
          Delete view
        </Button>
      )}
      {error && <span className="text-danger-fg ml-2">{error}</span>}

      {dialogOpen && (
        <SaveDialog
          name={name || activeView}
          existing={views.map((v) => v.name)}
          busy={busy}
          onChange={setName}
          onCancel={() => setDialogOpen(false)}
          onSubmit={() => {
            onSave((name || activeView).trim());
            setDialogOpen(false);
            setName("");
          }}
        />
      )}
    </div>
  );
}

function SaveDialog({
  name,
  existing,
  busy,
  onChange,
  onCancel,
  onSubmit,
}: {
  name: string;
  existing: string[];
  busy: boolean;
  onChange: (v: string) => void;
  onCancel: () => void;
  onSubmit: () => void;
}) {
  const trimmed = name.trim();
  const overwrite = existing.includes(trimmed);
  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onCancel();
      }}
      title="Save view"
      description="Captures the current search, label/assignee filters, sort, and grouping under a name. Saving over an existing name updates it."
      widthClass="max-w-md"
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            loading={busy}
            disabled={busy || trimmed === ""}
            onClick={onSubmit}
          >
            {overwrite ? "Update" : "Save"}
          </Button>
        </>
      }
    >
      <label className="block space-y-1">
        <span className="text-micro text-fg-muted">View name</span>
        <Input type="text" autoFocus value={name} onChange={(e) => onChange(e.target.value)} />
        {overwrite && (
          <span className="text-micro text-warning-fg">
            Updates the existing “{trimmed}” view.
          </span>
        )}
      </label>
    </Dialog>
  );
}
