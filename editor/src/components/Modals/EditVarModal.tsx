import { useState, useEffect } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import type { VarField, TypeExpr, Literal, LiteralKind } from "@/api/types";
import { TextField, SelectField, NumberField } from "@/components/Panels/forms/FormField";

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
      return { kind: "string" as LiteralKind, raw: `"${raw}"`, str_val: raw };
    case "json":
    case "string[]":
      return { kind: "string" as LiteralKind, raw, str_val: raw };
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
  const raw = lit.raw ?? "";
  if (raw.startsWith('"') && raw.endsWith('"')) return raw.slice(1, -1);
  return raw;
}

export default function EditVarModal({ name }: { name: string }) {
  const document = useDocumentStore((s) => s.document);
  const setVars = useDocumentStore((s) => s.setVars);
  const setEditingItem = useUIStore((s) => s.setEditingItem);

  const fields = document?.vars?.fields ?? [];
  const fieldIndex = fields.findIndex((f) => f.name === name);
  const field = fieldIndex >= 0 ? fields[fieldIndex] : undefined;

  // Local draft state
  const [draft, setDraft] = useState<VarField>(field ?? { name: "", type: "string" as TypeExpr });

  useEffect(() => {
    if (field) setDraft({ ...field });
  }, [field]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") setEditingItem(null);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [setEditingItem]);

  if (!field) return null;

  const hasChanges = JSON.stringify(draft) !== JSON.stringify(field);
  const nameError = !draft.name.trim() ? "Name cannot be empty" : null;
  const canSave = hasChanges && !nameError;

  const updateDraft = (updates: Partial<VarField>) => {
    setDraft((prev) => ({ ...prev, ...updates }));
  };

  const handleSave = () => {
    if (!canSave) return;
    const next = fields.map((f, i) => (i === fieldIndex ? draft : f));
    setVars({ fields: next });
    setEditingItem(null);
  };

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" onClick={() => setEditingItem(null)}>
      <div
        className="bg-gray-800 border border-gray-600 rounded-lg p-4 w-[400px] max-h-[80vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between mb-3">
          <h3 className="text-sm font-bold text-white">Edit Variable</h3>
          <button className="text-gray-400 hover:text-white text-lg px-1" onClick={() => setEditingItem(null)}>&times;</button>
        </div>
        <div className="space-y-3">
          <div>
            <TextField
              label="Name"
              value={draft.name}
              onChange={(v) => updateDraft({ name: v })}
              placeholder="var_name"
            />
            {nameError && <div className="text-[10px] text-red-400 mt-0.5">{nameError}</div>}
          </div>
          <SelectField
            label="Type"
            value={draft.type}
            onChange={(v) => updateDraft({ type: v as TypeExpr, default: undefined })}
            options={TYPE_OPTIONS}
          />
          <div>
            {draft.type === "bool" ? (
              <div className="flex items-center gap-2">
                <label className="text-xs text-gray-400">Default</label>
                <select
                  className="bg-gray-800 border border-gray-600 rounded px-2 py-1 text-sm text-white focus:border-blue-500 focus:outline-none"
                  value={draft.default ? displayDefault(draft.default) : ""}
                  onChange={(e) => updateDraft({ default: e.target.value === "" ? undefined : rawToLiteral("bool", e.target.value) })}
                >
                  <option value="">-- no default --</option>
                  <option value="true">true</option>
                  <option value="false">false</option>
                </select>
              </div>
            ) : draft.type === "int" || draft.type === "float" ? (
              <NumberField
                label="Default"
                value={draft.default ? (draft.type === "int" ? draft.default.int_val : draft.default.float_val) : undefined}
                onChange={(v) => updateDraft({ default: v !== undefined ? rawToLiteral(draft.type, String(v)) : undefined })}
                placeholder="Optional default"
              />
            ) : (
              <TextField
                label="Default"
                value={displayDefault(draft.default)}
                onChange={(v) => updateDraft({ default: rawToLiteral(draft.type, v) })}
                placeholder={draft.type === "json" ? '{"key": "value"}' : "Optional default"}
              />
            )}
          </div>
        </div>
        <div className="flex justify-end gap-2 mt-4 pt-3 border-t border-gray-700">
          <button
            className="bg-gray-700 hover:bg-gray-600 px-3 py-1.5 rounded text-xs text-white"
            onClick={() => setEditingItem(null)}
          >
            Cancel
          </button>
          <button
            className={`px-3 py-1.5 rounded text-xs text-white ${canSave ? "bg-blue-600 hover:bg-blue-700" : "bg-gray-600 cursor-not-allowed opacity-50"}`}
            onClick={handleSave}
            disabled={!canSave}
          >
            Save
          </button>
        </div>
      </div>
    </div>
  );
}
