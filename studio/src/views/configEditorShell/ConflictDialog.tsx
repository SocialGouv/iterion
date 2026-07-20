import { useMemo } from "react";

import { Button, Dialog } from "@/components/ui";

import { fieldChanged, normArray, type Draft, type EditableField, type FieldValue } from "./fieldModel";

// ---------------------------------------------------------------------------
// Conflict resolution — explicit user action, never a silent retry.
// ---------------------------------------------------------------------------

export function ConflictDialog({
  fields,
  yours,
  server,
  onCancel,
  onOverwrite,
  onAdoptServer,
}: {
  fields: EditableField[];
  yours: Draft;
  server: Draft;
  onCancel: () => void;
  onOverwrite: () => void;
  onAdoptServer: () => void;
}) {
  // Only surface the leaves that actually differ between the two versions.
  const diffed = useMemo(
    () => fields.filter((f) => fieldChanged(f, yours, server)),
    [fields, yours, server],
  );
  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onCancel();
      }}
      title="This config changed on the server"
      description="Someone else edited the file after you opened it. Review the differences below and choose how to proceed — no automatic retry."
      widthClass="max-w-3xl"
      stack="confirm"
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onCancel}>
            Keep editing
          </Button>
          <Button variant="secondary" size="sm" onClick={onAdoptServer}>
            Use the server version
          </Button>
          <Button variant="danger" size="sm" onClick={onOverwrite}>
            Overwrite with mine
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {diffed.map((f) => (
          <div key={f.path}>
            <div className="mb-1 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
              {f.parentLabel ? `${f.parentLabel} › ${f.leaf}` : f.leaf}
            </div>
            <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
              <ConflictPane title="Your draft" field={f} value={yours[f.path]} />
              <ConflictPane
                title="Server version (current)"
                field={f}
                value={server[f.path]}
                highlight
              />
            </div>
          </div>
        ))}
      </div>
    </Dialog>
  );
}

function ConflictPane({
  title,
  field,
  value,
  highlight = false,
}: {
  title: string;
  field: EditableField;
  value?: FieldValue;
  highlight?: boolean;
}) {
  return (
    <div
      className={`rounded-md border p-3 text-xs ${
        highlight ? "border-accent bg-accent-soft/50" : "border-border-default bg-surface-2"
      }`}
    >
      <h3 className="mb-1 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
        {title}
      </h3>
      {field.kind === "array" ? (
        (() => {
          const items = normArray(Array.isArray(value) ? value : []);
          return items.length === 0 ? (
            <p className="italic text-fg-subtle">empty</p>
          ) : (
            <ul className="space-y-0.5 font-mono">
              {items.map((it, i) => (
                <li key={i} className="truncate">
                  {it}
                </li>
              ))}
            </ul>
          );
        })()
      ) : (
        <pre className="whitespace-pre-wrap wrap-break-word font-mono text-fg-default">
          {(typeof value === "string" ? value : "") || (
            <span className="italic text-fg-subtle">empty</span>
          )}
        </pre>
      )}
    </div>
  );
}
