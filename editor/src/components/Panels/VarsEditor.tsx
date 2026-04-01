import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import type { VarField, TypeExpr, VarsBlock } from "@/api/types";
import { TextField, SelectField } from "./forms/FormField";

const TYPE_OPTIONS: { value: TypeExpr; label: string }[] = [
  { value: "string", label: "string" },
  { value: "bool", label: "bool" },
  { value: "int", label: "int" },
  { value: "float", label: "float" },
  { value: "json", label: "json" },
  { value: "string[]", label: "string[]" },
];

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
        <div key={i} className="flex gap-1 items-end mb-1">
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
              onChange={(v) => updateField(i, { type: v as TypeExpr })}
              options={TYPE_OPTIONS}
            />
          </div>
          <button className="text-red-400 hover:text-red-300 text-xs pb-2" onClick={() => removeField(i)}>
            x
          </button>
        </div>
      ))}
    </div>
  );
}
