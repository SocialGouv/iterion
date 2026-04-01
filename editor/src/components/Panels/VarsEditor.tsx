import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import type { VarField, TypeExpr, VarsBlock, Literal, LiteralKind } from "@/api/types";
import { TextField, SelectField, NumberField } from "./forms/FormField";

const TYPE_OPTIONS: { value: TypeExpr; label: string }[] = [
  { value: "string", label: "string" },
  { value: "bool", label: "bool" },
  { value: "int", label: "int" },
  { value: "float", label: "float" },
  { value: "json", label: "json" },
  { value: "string[]", label: "string[]" },
];

function rawToLiteral(type: TypeExpr, raw: string): Literal | undefined {
  if (raw === "") return undefined;
  switch (type) {
    case "string":
    case "json":
    case "string[]":
      return { kind: "string" as LiteralKind, raw: `"${raw}"`, str_val: raw };
    case "int":
      return { kind: "int" as LiteralKind, raw, int_val: parseInt(raw, 10) || 0 };
    case "float":
      return { kind: "float" as LiteralKind, raw, float_val: parseFloat(raw) || 0 };
    case "bool":
      return { kind: "bool" as LiteralKind, raw, bool_val: raw === "true" };
    default:
      return { kind: "string" as LiteralKind, raw: `"${raw}"`, str_val: raw };
  }
}

function displayDefault(lit: Literal | undefined): string {
  if (!lit) return "";
  if (lit.str_val !== undefined) return lit.str_val;
  if (lit.int_val !== undefined) return String(lit.int_val);
  if (lit.float_val !== undefined) return String(lit.float_val);
  if (lit.bool_val !== undefined) return String(lit.bool_val);
  // Fallback: strip quotes from raw
  const raw = lit.raw ?? "";
  if (raw.startsWith('"') && raw.endsWith('"')) return raw.slice(1, -1);
  return raw;
}

export default function VarsEditor() {
  const document = useDocumentStore((s) => s.document);
  const setVars = useDocumentStore((s) => s.setVars);
  const setWorkflowVars = useDocumentStore((s) => s.setWorkflowVars);

  const topLevelVars = document?.vars;
  const activeWorkflow = useActiveWorkflow();
  const workflowVars = activeWorkflow?.vars;

  return (
    <div className="p-3 text-sm">
      <h2 className="font-bold text-gray-300 mb-3">Variables</h2>

      <VarsSection
        title="Top-Level Vars"
        vars={topLevelVars}
        onChange={setVars}
        disabled={!document}
      />

      {activeWorkflow && (
        <VarsSection
          title={`Workflow "${activeWorkflow.name}" Vars`}
          vars={workflowVars}
          onChange={(v) => setWorkflowVars(activeWorkflow.name, v)}
          disabled={!document}
        />
      )}
    </div>
  );
}

function VarsSection({
  title,
  vars,
  onChange,
  disabled,
}: {
  title: string;
  vars: VarsBlock | undefined;
  onChange: (v: VarsBlock | undefined) => void;
  disabled: boolean;
}) {
  const fields = vars?.fields ?? [];

  const updateField = useCallback(
    (index: number, updates: Partial<VarField>) => {
      const next = fields.map((f, i) => (i === index ? { ...f, ...updates } : f));
      onChange({ fields: next });
    },
    [fields, onChange],
  );

  const addField = useCallback(() => {
    onChange({ fields: [...fields, { name: "", type: "string" as TypeExpr }] });
  }, [fields, onChange]);

  const removeField = useCallback(
    (index: number) => {
      const next = fields.filter((_, i) => i !== index);
      onChange(next.length > 0 ? { fields: next } : undefined);
    },
    [fields, onChange],
  );

  return (
    <div className="mb-4">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-xs text-gray-400 font-semibold">{title}</h3>
        <button
          className="text-blue-400 hover:text-blue-300 text-xs"
          onClick={addField}
          disabled={disabled}
        >
          + Add
        </button>
      </div>
      {fields.length === 0 && <p className="text-gray-500 text-xs">No variables defined.</p>}
      {fields.map((field, i) => (
        <div key={i} className="mb-2 p-2 bg-gray-800 rounded border border-gray-700">
          <div className="flex gap-1 items-end">
            <div className="flex-1">
              <TextField
                label="Name"
                value={field.name}
                onChange={(v) => updateField(i, { name: v })}
                placeholder="var_name"
              />
            </div>
            <div className="w-24">
              <SelectField
                label="Type"
                value={field.type}
                onChange={(v) => updateField(i, { type: v as TypeExpr, default: undefined })}
                options={TYPE_OPTIONS}
              />
            </div>
            <button className="text-red-400 hover:text-red-300 text-xs pb-2" onClick={() => removeField(i)}>
              x
            </button>
          </div>
          <div className="mt-1">
            {field.type === "bool" ? (
              <div className="flex items-center gap-2">
                <label className="text-xs text-gray-400">Default</label>
                <select
                  className="bg-gray-800 border border-gray-600 rounded px-2 py-1 text-sm text-white focus:border-blue-500 focus:outline-none"
                  value={field.default ? displayDefault(field.default) : ""}
                  onChange={(e) => updateField(i, { default: e.target.value === "" ? undefined : rawToLiteral("bool", e.target.value) })}
                >
                  <option value="">-- no default --</option>
                  <option value="true">true</option>
                  <option value="false">false</option>
                </select>
              </div>
            ) : field.type === "int" || field.type === "float" ? (
              <NumberField
                label="Default"
                value={field.default ? (field.type === "int" ? field.default.int_val : field.default.float_val) : undefined}
                onChange={(v) => updateField(i, { default: v !== undefined ? rawToLiteral(field.type, String(v)) : undefined })}
                placeholder="Optional default"
              />
            ) : (
              <TextField
                label="Default"
                value={displayDefault(field.default)}
                onChange={(v) => updateField(i, { default: rawToLiteral(field.type, v) })}
                placeholder={field.type === "json" ? '{"key": "value"}' : "Optional default"}
              />
            )}
          </div>
        </div>
      ))}
    </div>
  );
}
