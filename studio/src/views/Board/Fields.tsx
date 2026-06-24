// Fields view — operator surface for the board's custom-field schema.
//
// The native board carries a typed custom-field schema (board.Fields:
// text / number / enum / date / bool) that issues fill in and bots read.
// Until now the schema could only be seeded via `iterion issue board
// init` or a raw PUT /board; there was no way to add a field, fix a
// typo'd name, change a type, or drop a field from the studio. This view
// exposes the granular field ops the store grew alongside the column
// ones — add / edit / rename / delete / reorder — each cascading to
// issues server-side (rename rewrites the key, delete strips it) so the
// issues stay schema-valid.
//
// Mirrors Labels.tsx's structure (busy/error footer, confirm-on-delete).

import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";

import {
  addField,
  deleteField,
  getBoard,
  reorderFields,
  updateField,
  type NativeBoard,
  type NativeField,
  type NativeFieldType,
} from "@/api/native";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { TagInput } from "@/components/ui/TagInput";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";

const FIELD_TYPES: NativeFieldType[] = ["text", "number", "enum", "date", "bool"];

type DialogState =
  | { kind: "none" }
  | { kind: "add" }
  | { kind: "edit"; field: NativeField };

export default function FieldsView() {
  return (
    <ErrorBoundary area="Fields view">
      <FieldsViewInner />
    </ErrorBoundary>
  );
}

function FieldsViewInner() {
  const [, setLocation] = useLocation();
  const [board, setBoard] = useState<NativeBoard | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [dialog, setDialog] = useState<DialogState>({ kind: "none" });
  const action = useAsyncAction();
  const { confirm, dialog: confirmDialog } = useConfirm();

  const refresh = useCallback(async () => {
    try {
      setBoard(await getBoard());
      setLoadError(null);
    } catch (e) {
      setLoadError(errorMessage(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const fields = useMemo(() => board?.fields ?? [], [board]);

  const onApply = useCallback(
    async (op: () => Promise<unknown>) => {
      const ok = await action.run(async () => {
        await op();
        await refresh();
        return true;
      });
      if (ok) setDialog({ kind: "none" });
    },
    [action, refresh],
  );

  const onDelete = useCallback(
    async (f: NativeField) => {
      const ok = await confirm({
        title: `Delete field “${f.display ?? f.name}”?`,
        message: `Removes the field from the board schema and strips its value from every issue that carries it. This cannot be undone.`,
        confirmLabel: "Delete",
        confirmVariant: "danger",
      });
      if (!ok) return;
      await action.run(async () => {
        await deleteField(f.name);
        await refresh();
      });
    },
    [action, confirm, refresh],
  );

  const onMove = useCallback(
    (name: string, dir: "up" | "down") => {
      const names = fields.map((f) => f.name);
      const i = names.indexOf(name);
      const j = dir === "up" ? i - 1 : i + 1;
      const a = names[i];
      const b = names[j];
      if (a === undefined || b === undefined) return;
      names[i] = b;
      names[j] = a;
      void action.run(async () => {
        await reorderFields(names);
        await refresh();
        return true;
      });
    },
    [fields, action, refresh],
  );

  return (
    <div className="h-full overflow-auto p-4 space-y-3 text-label">
      <header className="flex items-baseline gap-3">
        <h1 className="text-lg font-semibold text-fg-default">Board fields</h1>
        <span className="text-fg-muted text-micro">
          {fields.length} custom field{fields.length === 1 ? "" : "s"}
        </span>
        <div className="ml-auto flex items-center gap-2">
          <Button variant="primary" size="sm" onClick={() => setDialog({ kind: "add" })}>
            + Add field
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setLocation("/board")}>
            ← Back to board
          </Button>
        </div>
      </header>

      <p className="text-fg-muted text-micro max-w-3xl">
        Custom fields extend each issue with typed metadata (severity, ETA,
        owner…). Bots read and write them via the board tools. Renaming a field
        rewrites the key on every issue; deleting it strips the value — issues
        stay schema-valid either way.
      </p>

      {(action.error || loadError) && (
        <div className="text-danger-fg text-micro" role="alert">
          {action.error ?? loadError}
        </div>
      )}

      {!board && <EmptyState message="Loading…" />}

      {board && fields.length === 0 && (
        <p className="text-fg-muted text-micro italic">
          No custom fields yet. Add one to attach typed metadata to issues.
        </p>
      )}

      {fields.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-body border border-border-subtle">
            <thead>
              <tr className="bg-surface-1 text-fg-muted text-left">
                <th className="px-2 py-1 font-medium">Name</th>
                <th className="px-2 py-1 font-medium w-24">Type</th>
                <th className="px-2 py-1 font-medium w-20">Required</th>
                <th className="px-2 py-1 font-medium">Values</th>
                <th className="px-2 py-1 font-medium w-56">Actions</th>
              </tr>
            </thead>
            <tbody>
              {fields.map((f, i) => (
                <tr key={f.name} className="border-t border-border-subtle hover:bg-surface-1/40">
                  <td className="px-2 py-1 font-mono text-fg-default">
                    {f.name}
                    {f.display && (
                      <span className="ml-2 text-fg-muted font-sans">{f.display}</span>
                    )}
                  </td>
                  <td className="px-2 py-1 text-fg-default">{f.type}</td>
                  <td className="px-2 py-1 text-fg-muted">{f.required ? "yes" : "—"}</td>
                  <td className="px-2 py-1 text-fg-muted truncate max-w-xs">
                    {f.type === "enum" ? (f.enum_values ?? []).join(", ") : "—"}
                  </td>
                  <td className="px-2 py-1">
                    <div className="flex gap-1.5 items-center">
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={i === 0}
                        onClick={() => onMove(f.name, "up")}
                        title="Move up"
                      >
                        ↑
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={i === fields.length - 1}
                        onClick={() => onMove(f.name, "down")}
                        title="Move down"
                      >
                        ↓
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setDialog({ kind: "edit", field: f })}
                      >
                        edit
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-danger-fg hover:text-danger"
                        onClick={() => void onDelete(f)}
                      >
                        delete
                      </Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {dialog.kind === "add" && (
        <FieldDialog
          mode="add"
          existingNames={fields.map((f) => f.name)}
          busy={action.busy}
          onCancel={() => setDialog({ kind: "none" })}
          onSubmit={(field) => void onApply(() => addField(field))}
        />
      )}
      {dialog.kind === "edit" && (
        <FieldDialog
          mode="edit"
          field={dialog.field}
          existingNames={fields.map((f) => f.name).filter((n) => n !== dialog.field.name)}
          busy={action.busy}
          onCancel={() => setDialog({ kind: "none" })}
          onSubmit={(field) =>
            void onApply(() =>
              updateField(dialog.field.name, {
                name: field.name !== dialog.field.name ? field.name : undefined,
                display: field.display ?? "",
                type: field.type,
                required: field.required ?? false,
                enum_values: field.type === "enum" ? field.enum_values ?? [] : [],
              }),
            )
          }
        />
      )}
      {confirmDialog}
    </div>
  );
}

function FieldDialog({
  mode,
  field,
  existingNames,
  busy,
  onCancel,
  onSubmit,
}: {
  mode: "add" | "edit";
  field?: NativeField;
  existingNames: string[];
  busy: boolean;
  onCancel: () => void;
  onSubmit: (field: NativeField) => void;
}) {
  const [name, setName] = useState(field?.name ?? "");
  const [display, setDisplay] = useState(field?.display ?? "");
  const [type, setType] = useState<NativeFieldType>(field?.type ?? "text");
  const [required, setRequired] = useState(!!field?.required);
  const [enumValues, setEnumValues] = useState<string[]>(field?.enum_values ?? []);

  const trimmed = name.trim();
  const duplicate = existingNames.includes(trimmed);
  const enumInvalid = type === "enum" && enumValues.length === 0;
  const invalid = trimmed === "" || duplicate || enumInvalid;

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onCancel();
      }}
      title={mode === "add" ? "Add field" : `Edit “${field?.display ?? field?.name}”`}
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
            disabled={busy || invalid}
            onClick={() =>
              onSubmit({
                name: trimmed,
                display: display.trim() || undefined,
                type,
                required: required || undefined,
                enum_values: type === "enum" ? enumValues : undefined,
              })
            }
          >
            {busy ? "…" : mode === "add" ? "Add field" : "Save"}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <label className="block space-y-1">
          <span className="text-micro text-fg-muted">
            Machine name (renaming rewrites the key on every issue)
          </span>
          <Input
            type="text"
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="font-mono"
            error={duplicate}
          />
          {duplicate && (
            <span className="text-micro text-danger-fg">
              A field named “{trimmed}” already exists.
            </span>
          )}
        </label>
        <label className="block space-y-1">
          <span className="text-micro text-fg-muted">Display name (optional)</span>
          <Input type="text" value={display} onChange={(e) => setDisplay(e.target.value)} />
        </label>
        <label className="block space-y-1">
          <span className="text-micro text-fg-muted">Type</span>
          <Select value={type} onChange={(e) => setType(e.target.value as NativeFieldType)}>
            {FIELD_TYPES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </Select>
        </label>
        {type === "enum" && (
          <div className="space-y-1">
            <span className="text-micro text-fg-muted">Enum values</span>
            <TagInput value={enumValues} onChange={setEnumValues} placeholder="add a value…" />
            {enumInvalid && (
              <span className="text-micro text-danger-fg">
                An enum field needs at least one value.
              </span>
            )}
          </div>
        )}
        <label className="flex items-center gap-2">
          <Checkbox checked={required} onChange={(e) => setRequired(e.target.checked)} />
          <span className="text-micro text-fg-default">Required on every issue</span>
        </label>
      </div>
    </Dialog>
  );
}
