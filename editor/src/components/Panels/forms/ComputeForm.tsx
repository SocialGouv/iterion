import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useSchemaPromptCreators } from "@/hooks/useSchemaPromptCreators";
import type { ComputeDecl, AwaitMode, ComputeExpr } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import { AWAIT_OPTIONS } from "@/lib/dslOptions";

import {
  CommittedTextField,
  SelectField,
  SelectFieldWithCreate,
  TextField,
} from "./FormField";

interface Props {
  decl: ComputeDecl;
}

const HEADER_COLOR = "#0EA5E9";

/** Form for compute nodes — deterministic expression evaluator (no LLM,
 *  no shell). Each entry maps a key in the output schema to a raw
 *  expression source. The compiler parses these at validation time. */
export default function ComputeForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateCompute = useDocumentStore((s) => s.updateCompute);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const { createSchema } = useSchemaPromptCreators();

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));
  const outputSchemaFields =
    document?.schemas.find((s) => s.name === decl.output)?.fields ?? [];

  const setExprAt = (idx: number, patch: Partial<ComputeExpr>) => {
    const next = decl.expr.map((e, i) => (i === idx ? { ...e, ...patch } : e));
    updateCompute(decl.name, { expr: next });
  };
  const removeExprAt = (idx: number) => {
    updateCompute(decl.name, { expr: decl.expr.filter((_, i) => i !== idx) });
  };
  const addExpr = () => {
    updateCompute(decl.name, { expr: [...decl.expr, { key: "", expr: "" }] });
  };

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: HEADER_COLOR + "22", borderLeft: `3px solid ${HEADER_COLOR}` }}
      >
        <span className="text-base">{"\u{03A3}"}</span>
        <span
          className="text-xs font-bold uppercase tracking-wide"
          style={{ color: HEADER_COLOR }}
        >
          Compute
        </span>
      </div>
      <CommittedTextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
        onCommit={(v) => setSelectedNode(v)}
        validate={(v) => {
          if (!v.trim()) return "Name cannot be empty";
          if (/\s/.test(v)) return "Name cannot contain spaces";
          const names = getAllNodeNames(document!);
          names.delete(decl.name);
          if (names.has(v)) return "Name already exists";
          return null;
        }}
      />
      <SelectFieldWithCreate
        label="Input Schema"
        value={decl.input ?? ""}
        onChange={(v) => updateCompute(decl.name, { input: v || undefined })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- none --"
        onCreate={createSchema}
        help="Optional. Lets expressions reference {{input.field}}."
      />
      <SelectFieldWithCreate
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateCompute(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
        help="Defines the keys produced by the expressions below."
      />
      <SelectField
        label="Await"
        value={decl.await ?? "none"}
        onChange={(v) =>
          updateCompute(decl.name, {
            await: (v === "none" ? undefined : v) as AwaitMode | undefined,
          })
        }
        options={AWAIT_OPTIONS}
      />

      <div className="border-t border-border-default pt-2 mt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-fg-subtle font-semibold">
            Expressions{" "}
            <span
              className="text-fg-subtle hover:text-fg-muted cursor-help"
              title="Each entry assigns one output schema field. Expressions can reference vars / input / outputs / artifacts / loop / run namespaces."
            >
              ?
            </span>
          </span>
          <button
            type="button"
            className="text-xs text-accent hover:text-accent"
            onClick={addExpr}
          >
            + Add
          </button>
        </div>
        {decl.expr.length === 0 ? (
          <p className="text-[10px] text-fg-subtle italic">
            No expressions yet. Add one per output field you want to compute.
          </p>
        ) : (
          <ul className="space-y-2">
            {decl.expr.map((e, i) => (
              <li
                key={i}
                className="border border-border-default rounded p-2 bg-surface-1/40"
              >
                <div className="flex items-center justify-between mb-1">
                  <span className="text-[10px] text-fg-subtle font-mono">#{i}</span>
                  <button
                    type="button"
                    className="text-[10px] text-danger hover:text-danger-fg"
                    onClick={() => removeExprAt(i)}
                  >
                    Remove
                  </button>
                </div>
                {outputSchemaFields.length > 0 ? (
                  <SelectField
                    label="Key (output field)"
                    value={e.key}
                    onChange={(v) => setExprAt(i, { key: v })}
                    options={outputSchemaFields.map((f) => ({
                      value: f.name,
                      label: `${f.name} (${f.type})`,
                    }))}
                    allowEmpty
                    emptyLabel="-- select field --"
                  />
                ) : (
                  <TextField
                    label="Key"
                    value={e.key}
                    onChange={(v) => setExprAt(i, { key: v })}
                    placeholder="output field name"
                  />
                )}
                <TextField
                  label="Expression"
                  value={e.expr}
                  onChange={(v) => setExprAt(i, { expr: v })}
                  placeholder="e.g. input.count >= vars.loop_count"
                  multiline
                  rows={2}
                  refContext={{ kind: "node-prompt", nodeId: decl.name }}
                />
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
