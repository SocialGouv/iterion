// Column-management dialogs for the native board — the operator surface
// for the editable columns that bring the board to GitHub-Projects-style
// parity. Three dialogs:
//
//   AddColumnDialog    — create a new column (machine name + display +
//                        color + eligible/terminal flags).
//   EditColumnDialog   — edit an existing column's display/color/flags,
//                        and rename it inline (cascades to issues; a note
//                        shows how many will move).
//   DeleteColumnDialog — remove a column. A non-empty column requires a
//                        migration target (issues move there first); the
//                        last remaining column cannot be deleted.
//
// All three reuse the design-system Dialog/Input/Select/Checkbox/Button
// primitives and follow Labels.tsx's busy/disabled footer pattern.

import { useState } from "react";

import type { NativeState } from "@/api/native";
import { Checkbox } from "@/components/ui/Checkbox";
import { Dialog } from "@/components/ui/Dialog";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

import { BOARD_PALETTE, defaultStateColor } from "./boardShared";
import { ModalActions } from "./ModalActions";

// Shared swatch grid + flags editor used by Add and Edit.
function ColumnAppearance({
  display,
  color,
  eligible,
  terminal,
  fallbackName,
  onChange,
}: {
  display: string;
  color: string; // "" = auto (defaultStateColor fallback)
  eligible: boolean;
  terminal: boolean;
  fallbackName: string;
  onChange: (patch: {
    display?: string;
    color?: string;
    eligible?: boolean;
    terminal?: boolean;
  }) => void;
}) {
  const effectiveAuto = defaultStateColor(fallbackName, eligible, terminal);
  return (
    <>
      <label className="block space-y-1">
        <span className="text-micro text-fg-muted">Display name</span>
        <Input
          type="text"
          value={display}
          placeholder={fallbackName}
          onChange={(e) => onChange({ display: e.target.value })}
        />
      </label>

      <div className="space-y-1">
        <span className="text-micro text-fg-muted">Color</span>
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            onClick={() => onChange({ color: "" })}
            title="Auto (semantic default)"
            className={`h-6 w-6 rounded-full border-2 ${
              color === "" ? "border-accent ring-2 ring-accent/40" : "border-border-default"
            }`}
            style={{ backgroundColor: effectiveAuto, opacity: 0.5 }}
            aria-label="Automatic color"
          />
          {BOARD_PALETTE.map((p) => (
            <button
              key={p.value}
              type="button"
              onClick={() => onChange({ color: p.value })}
              title={p.label}
              className={`h-6 w-6 rounded-full border-2 ${
                color === p.value
                  ? "border-accent ring-2 ring-accent/40"
                  : "border-border-default"
              }`}
              style={{ backgroundColor: p.value }}
              aria-label={`${p.label} color`}
            />
          ))}
        </div>
      </div>

      <div className="space-y-2 rounded border border-border-subtle p-2">
        <label className="flex items-start gap-2">
          <Checkbox
            checked={eligible}
            onChange={(e) => onChange({ eligible: e.target.checked })}
            className="mt-0.5"
          />
          <span className="text-micro">
            <span className="text-fg-default font-medium">Eligible</span> — the
            dispatcher may pick up issues in this column (the “Let’s go” lane is
            the first eligible, non-terminal column).
          </span>
        </label>
        <label className="flex items-start gap-2">
          <Checkbox
            checked={terminal}
            onChange={(e) => onChange({ terminal: e.target.checked })}
            className="mt-0.5"
          />
          <span className="text-micro">
            <span className="text-fg-default font-medium">Terminal</span> — an
            end state (done/blocked). Issues here are treated as “no more work”.
          </span>
        </label>
        {eligible && terminal && (
          <p className="text-micro text-warning-fg">
            A terminal column is never the dispatch lane even when eligible.
          </p>
        )}
      </div>
    </>
  );
}

export function AddColumnDialog({
  existingNames,
  busy,
  error,
  onCancel,
  onSubmit,
}: {
  existingNames: string[];
  busy: boolean;
  error: string | null;
  onCancel: () => void;
  onSubmit: (state: NativeState) => void;
}) {
  const [name, setName] = useState("");
  const [display, setDisplay] = useState("");
  const [color, setColor] = useState("");
  const [eligible, setEligible] = useState(false);
  const [terminal, setTerminal] = useState(false);

  const trimmed = name.trim();
  const duplicate = existingNames.includes(trimmed);
  const invalid = trimmed === "" || duplicate;

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onCancel();
      }}
      title="Add column"
      description="Columns are board states. The new column is appended on the right — drag its handle or use Move to reorder."
      widthClass="max-w-md"
      footer={
        <ModalActions
          onCancel={onCancel}
          primaryLabel="Add column"
          primaryVariant="primary"
          onPrimary={() =>
            onSubmit({
              name: trimmed,
              display: display.trim() || undefined,
              color: color || undefined,
              eligible: eligible || undefined,
              terminal: terminal || undefined,
            })
          }
          busy={busy}
          disabled={invalid}
        />
      }
    >
      <div className="space-y-3">
        <label className="block space-y-1">
          <span className="text-micro text-fg-muted">
            Machine name (lowercase, used by bots &amp; the dispatcher)
          </span>
          <Input
            type="text"
            autoFocus
            value={name}
            placeholder="e.g. triage"
            onChange={(e) => setName(e.target.value)}
            className="font-mono"
            error={duplicate}
          />
          {duplicate && (
            <span className="text-micro text-danger-fg">
              A column named “{trimmed}” already exists.
            </span>
          )}
        </label>
        <ColumnAppearance
          display={display}
          color={color}
          eligible={eligible}
          terminal={terminal}
          fallbackName={trimmed || "new"}
          onChange={(p) => {
            if (p.display !== undefined) setDisplay(p.display);
            if (p.color !== undefined) setColor(p.color);
            if (p.eligible !== undefined) setEligible(p.eligible);
            if (p.terminal !== undefined) setTerminal(p.terminal);
          }}
        />
        {error && (
          <p className="text-micro text-danger-fg" role="alert">
            {error}
          </p>
        )}
      </div>
    </Dialog>
  );
}

export function EditColumnDialog({
  state,
  issueCount,
  existingNames,
  busy,
  error,
  onCancel,
  onSubmit,
}: {
  state: NativeState;
  issueCount: number;
  existingNames: string[]; // other columns' names (excludes this one)
  busy: boolean;
  error: string | null;
  // patch.name set only when the machine name changed (triggers a rename).
  onCancel: () => void;
  onSubmit: (patch: {
    name?: string;
    display?: string;
    color?: string;
    eligible?: boolean;
    terminal?: boolean;
  }) => void;
}) {
  const [name, setName] = useState(state.name);
  const [display, setDisplay] = useState(state.display ?? "");
  const [color, setColor] = useState(state.color ?? "");
  const [eligible, setEligible] = useState(!!state.eligible);
  const [terminal, setTerminal] = useState(!!state.terminal);

  const trimmed = name.trim();
  const renamed = trimmed !== state.name;
  const duplicate = renamed && existingNames.includes(trimmed);
  const invalid = trimmed === "" || duplicate;

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onCancel();
      }}
      title={`Edit “${state.display ?? state.name}”`}
      widthClass="max-w-md"
      footer={
        <ModalActions
          onCancel={onCancel}
          primaryLabel="Save"
          primaryVariant="primary"
          onPrimary={() =>
            onSubmit({
              name: renamed ? trimmed : undefined,
              display: display.trim(),
              color,
              eligible,
              terminal,
            })
          }
          busy={busy}
          disabled={invalid}
        />
      }
    >
      <div className="space-y-3">
        <label className="block space-y-1">
          <span className="text-micro text-fg-muted">
            Machine name (renaming moves every issue in this column)
          </span>
          <Input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="font-mono"
            error={duplicate}
          />
          {duplicate && (
            <span className="text-micro text-danger-fg">
              A column named “{trimmed}” already exists — delete-with-migrate to
              merge columns.
            </span>
          )}
          {renamed && !duplicate && issueCount > 0 && (
            <span className="text-micro text-warning-fg">
              {issueCount} issue{issueCount === 1 ? "" : "s"} will move to “{trimmed}”.
            </span>
          )}
        </label>
        <ColumnAppearance
          display={display}
          color={color}
          eligible={eligible}
          terminal={terminal}
          fallbackName={trimmed || state.name}
          onChange={(p) => {
            if (p.display !== undefined) setDisplay(p.display);
            if (p.color !== undefined) setColor(p.color);
            if (p.eligible !== undefined) setEligible(p.eligible);
            if (p.terminal !== undefined) setTerminal(p.terminal);
          }}
        />
        {error && (
          <p className="text-micro text-danger-fg" role="alert">
            {error}
          </p>
        )}
      </div>
    </Dialog>
  );
}

export function DeleteColumnDialog({
  state,
  issueCount,
  otherStates,
  isLast,
  busy,
  error,
  onCancel,
  onSubmit,
}: {
  state: NativeState;
  issueCount: number;
  otherStates: NativeState[];
  isLast: boolean;
  busy: boolean;
  error: string | null;
  onSubmit: (migrateTo: string | undefined) => void;
  onCancel: () => void;
}) {
  const [migrateTo, setMigrateTo] = useState("");
  const needsTarget = issueCount > 0;
  const disabled = isLast || (needsTarget && migrateTo === "");

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onCancel();
      }}
      title={`Delete “${state.display ?? state.name}”?`}
      widthClass="max-w-md"
      footer={
        <ModalActions
          onCancel={onCancel}
          primaryLabel="Delete column"
          primaryVariant="danger"
          onPrimary={() => onSubmit(needsTarget ? migrateTo : undefined)}
          busy={busy}
          disabled={disabled}
        />
      }
    >
      <div className="space-y-3 text-body">
        {isLast ? (
          <p className="text-warning-fg">
            This is the only column — a board must keep at least one. Add another
            column first.
          </p>
        ) : needsTarget ? (
          <>
            <p className="text-fg-default">
              This column holds {issueCount} issue{issueCount === 1 ? "" : "s"}.
              Choose where to move {issueCount === 1 ? "it" : "them"} before the
              column is removed.
            </p>
            <label className="block space-y-1">
              <span className="text-micro text-fg-muted">Move issues to</span>
              <Select
                value={migrateTo}
                onChange={(e) => setMigrateTo(e.target.value)}
                aria-label="Migration target column"
              >
                <option value="" disabled>
                  Select a column…
                </option>
                {otherStates.map((s) => (
                  <option key={s.name} value={s.name}>
                    {s.display ?? s.name}
                  </option>
                ))}
              </Select>
            </label>
          </>
        ) : (
          <p className="text-fg-default">
            This column is empty and will be removed. This cannot be undone.
          </p>
        )}
        {error && (
          <p className="text-micro text-danger-fg" role="alert">
            {error}
          </p>
        )}
      </div>
    </Dialog>
  );
}
