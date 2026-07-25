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

import { useCallback, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation } from "wouter";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";

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
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { TagInput } from "@/components/ui/TagInput";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";

import { moveInArray } from "./boardShared";
import { ModalActions } from "./ModalActions";

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
  const [dialog, setDialog] = useState<DialogState>({ kind: "none" });
  const action = useAsyncAction();
  const { confirm, dialog: confirmDialog } = useConfirm();

  const queryClient = useQueryClient();
  const boardQuery = useQuery<NativeBoard>({
    queryKey: ["board"],
    queryFn: () => getBoard(),
  });
  const board = boardQuery.data ?? null;
  const loadError = boardQuery.error ? errorMessage(boardQuery.error) : null;

  // Field ops mutate the board schema — re-pull it after each write.
  const refresh = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["board"] }),
    [queryClient],
  );

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
      const next = moveInArray(
        fields.map((f) => f.name),
        name,
        dir === "up" ? -1 : 1,
      );
      if (!next) return;
      void action.run(async () => {
        await reorderFields(next);
        await refresh();
        return true;
      });
    },
    [fields, action, refresh],
  );

  useHeaderSlot({
    left: (
      <span className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
        <Link href="/board" className="text-fg-muted hover:text-fg-default hover:underline">
          Board
        </Link>
        <span className="text-fg-subtle">/</span>
        <span>Fields</span>
      </span>
    ),
  });

  return (
    <div className="h-full overflow-auto p-4 space-y-3 text-label">
      <header className="flex items-baseline gap-3">
        <h1 className="text-headline font-semibold tracking-tight text-fg-default">Board fields</h1>
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

      {!board && <TableSkeleton />}

      {board && fields.length === 0 && (
        <EmptyState
          title="No custom fields yet"
          message="Add one to attach typed metadata (severity, ETA, owner…) to every issue on the board."
          action={
            <Button variant="primary" size="sm" onClick={() => setDialog({ kind: "add" })}>
              + Add field
            </Button>
          }
        />
      )}

      {fields.length > 0 && (
        <Table
          caption="Custom fields on the board schema"
          className="border border-border-subtle"
        >
          <THead className="bg-surface-1">
            <Th>Name</Th>
            <Th className="w-24">Type</Th>
            <Th className="w-20">Required</Th>
            <Th>Values</Th>
            <Th className="w-56">Actions</Th>
          </THead>
          <TBody>
            {fields.map((f, i) => (
              <Tr key={f.name}>
                <Td className="font-mono text-fg-default">
                  {f.name}
                  {f.display && (
                    <span className="ml-2 text-fg-muted font-sans">{f.display}</span>
                  )}
                </Td>
                <Td className="text-fg-default">{f.type}</Td>
                <Td className="text-fg-muted">{f.required ? "yes" : "—"}</Td>
                <Td className="text-fg-muted truncate max-w-xs">
                  {f.type === "enum" ? (f.enum_values ?? []).join(", ") : "—"}
                </Td>
                <Td>
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
                  </Td>
                </Tr>
              ))}
          </TBody>
        </Table>
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
        <ModalActions
          onCancel={onCancel}
          primaryLabel={mode === "add" ? "Add field" : "Save"}
          busy={busy}
          disabled={invalid}
          onPrimary={() =>
            onSubmit({
              name: trimmed,
              display: display.trim() || undefined,
              type,
              required: required || undefined,
              enum_values: type === "enum" ? enumValues : undefined,
            })
          }
        />
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
