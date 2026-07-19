// Extracted from BotBuilder/index.tsx to keep that file focused.
// The vars editor — row-per-variable grid with inline name/type/default/
// description editing and per-row validation feedback.

import { Cross1Icon, PlusIcon } from "@radix-ui/react-icons";

import { Button, Card, IconButton, Input, Select } from "@/components/ui";

import { VAR_TYPES, type BuilderDraft, type PatchDraft, type VarRow, type VarType } from "./model";
import SectionTitle from "./SectionTitle";
import { isValidVarName } from "./slug";

export default function VarsEditorCard({
  draft,
  patch,
}: {
  draft: BuilderDraft;
  patch: PatchDraft;
}) {
  const setRow = (i: number, p: Partial<VarRow>) =>
    patch({ vars: draft.vars.map((v, j) => (j === i ? { ...v, ...p } : v)) });
  const removeRow = (i: number) => patch({ vars: draft.vars.filter((_, j) => j !== i) });
  const addRow = () =>
    patch({ vars: [...draft.vars, { name: "", type: "string", default: "", description: "" }] });

  const names = draft.vars.map((v) => v.name.trim());

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle>Variables</SectionTitle>
        <Button variant="secondary" size="sm" onClick={addRow}>
          <PlusIcon className="mr-1 h-3 w-3" />
          Add variable
        </Button>
      </div>
      {draft.vars.length === 0 ? (
        <p className="mt-1 text-caption text-fg-subtle">
          Optional launch-time inputs (<code className="font-mono">{"{{vars.name}}"}</code> in the
          instructions).
        </p>
      ) : (
        <div className="mt-2 flex flex-col gap-2">
          {draft.vars.map((v, i) => {
            const trimmed = v.name.trim();
            const rowActive = trimmed !== "" || v.default !== "" || v.description !== "";
            const nameInvalid = rowActive && !isValidVarName(trimmed);
            const duplicate =
              rowActive && trimmed !== "" && names.filter((n) => n === trimmed).length > 1;
            return (
              <div key={i} className="rounded-md border border-border-default bg-surface-2 p-2">
                <div className="grid grid-cols-[minmax(0,1fr)_96px_minmax(0,1fr)_auto] items-center gap-2">
                  <Input
                    type="text"
                    value={v.name}
                    onChange={(e) => setRow(i, { name: e.target.value })}
                    placeholder="name"
                    aria-label={`Variable ${i + 1} name`}
                    size="sm"
                    className="font-mono"
                    error={nameInvalid || duplicate}
                  />
                  <Select
                    value={v.type}
                    onChange={(e) => setRow(i, { type: e.currentTarget.value as VarType })}
                    aria-label={`Variable ${i + 1} type`}
                  >
                    {VAR_TYPES.map((t) => (
                      <option key={t} value={t}>
                        {t}
                      </option>
                    ))}
                  </Select>
                  <Input
                    type="text"
                    value={v.default}
                    onChange={(e) => setRow(i, { default: e.target.value })}
                    placeholder="default (empty = required)"
                    aria-label={`Variable ${i + 1} default`}
                    size="sm"
                    className="font-mono"
                  />
                  <IconButton
                    label={`Remove variable ${trimmed || i + 1}`}
                    size="sm"
                    variant="ghost"
                    onClick={() => removeRow(i)}
                  >
                    <Cross1Icon className="h-3 w-3" />
                  </IconButton>
                </div>
                <Input
                  type="text"
                  value={v.description}
                  onChange={(e) => setRow(i, { description: e.target.value })}
                  placeholder="description (optional)"
                  aria-label={`Variable ${i + 1} description`}
                  size="sm"
                  className="mt-1.5"
                />
                {(nameInvalid || duplicate) && (
                  <p className="mt-1 text-caption text-danger-fg" role="alert">
                    {nameInvalid
                      ? "Var names must match ^[a-z_][a-z0-9_]*$ (snake_case)."
                      : "Duplicate var name."}
                  </p>
                )}
              </div>
            );
          })}
        </div>
      )}
    </Card>
  );
}
