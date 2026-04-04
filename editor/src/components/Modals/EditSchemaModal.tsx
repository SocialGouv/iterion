import { useState, useEffect } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import type { SchemaField, FieldType } from "@/api/types";
import { TextField, SelectField, TagListField } from "@/components/Panels/forms/FormField";

const FIELD_TYPES: { value: FieldType; label: string }[] = [
  { value: "string", label: "string" },
  { value: "bool", label: "bool" },
  { value: "int", label: "int" },
  { value: "float", label: "float" },
  { value: "json", label: "json" },
  { value: "string[]", label: "string[]" },
];

export default function EditSchemaModal({ name }: { name: string }) {
  const document = useDocumentStore((s) => s.document);
  const updateSchema = useDocumentStore((s) => s.updateSchema);
  const renameSchema = useDocumentStore((s) => s.renameSchema);
  const setEditingItem = useUIStore((s) => s.setEditingItem);

  const schema = document?.schemas?.find((s) => s.name === name);
  const allSchemaNames = document?.schemas?.map((s) => s.name) ?? [];

  // Local draft state
  const [draftName, setDraftName] = useState(name);
  const [draftFields, setDraftFields] = useState<SchemaField[]>(schema?.fields ?? []);

  useEffect(() => {
    if (schema) {
      setDraftName(schema.name);
      setDraftFields(schema.fields.map((f) => ({ ...f })));
    }
  }, [schema]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") setEditingItem(null);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [setEditingItem]);

  if (!schema) return null;

  const nameError = (() => {
    if (!draftName.trim()) return "Name cannot be empty";
    const others = new Set(allSchemaNames);
    others.delete(name);
    if (others.has(draftName)) return "Schema name already exists";
    return null;
  })();

  const fieldsChanged = JSON.stringify(draftFields) !== JSON.stringify(schema.fields);
  const hasChanges = draftName !== name || fieldsChanged;
  const canSave = hasChanges && !nameError;

  const updateField = (index: number, updates: Partial<SchemaField>) => {
    setDraftFields((prev) => prev.map((f, i) => (i === index ? { ...f, ...updates } : f)));
  };

  const addField = () => {
    setDraftFields((prev) => [...prev, { name: "", type: "string" as FieldType }]);
  };

  const removeField = (index: number) => {
    setDraftFields((prev) => prev.filter((_, i) => i !== index));
  };

  const handleSave = () => {
    if (!canSave) return;
    if (draftName !== name) renameSchema(name, draftName);
    if (fieldsChanged) updateSchema(draftName, { fields: draftFields });
    setEditingItem(null);
  };

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" onClick={() => setEditingItem(null)}>
      <div
        className="bg-gray-800 border border-gray-600 rounded-lg p-4 w-[500px] max-h-[80vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between mb-3">
          <h3 className="text-sm font-bold text-white">Edit Schema</h3>
          <button className="text-gray-400 hover:text-white text-lg px-1" onClick={() => setEditingItem(null)}>&times;</button>
        </div>
        <div className="space-y-3">
          <div>
            <TextField
              label="Name"
              value={draftName}
              onChange={setDraftName}
              placeholder="schema_name"
            />
            {nameError && <div className="text-[10px] text-red-400 mt-0.5">{nameError}</div>}
          </div>
          <div>
            <label className="text-xs text-gray-400 mb-1 block">Fields</label>
            <div className="space-y-2">
              {draftFields.map((field, i) => (
                <div key={i}>
                  <div className="flex gap-1 items-end">
                    <div className="flex-1">
                      <TextField
                        label="Field"
                        value={field.name}
                        onChange={(v) => updateField(i, { name: v })}
                        placeholder="field_name"
                      />
                    </div>
                    <div className="w-24">
                      <SelectField
                        label="Type"
                        value={field.type}
                        onChange={(v) => updateField(i, { type: v as FieldType, enum_values: v === "string" ? field.enum_values : undefined })}
                        options={FIELD_TYPES}
                      />
                    </div>
                    <button className="text-red-400 hover:text-red-300 text-xs pb-2" onClick={() => removeField(i)}>
                      x
                    </button>
                  </div>
                  {field.type === "string" && (
                    <div className="ml-2 mt-1">
                      <TagListField
                        label={`${field.name || "field"} enum values`}
                        values={field.enum_values ?? []}
                        onChange={(v) => updateField(i, { enum_values: v.length > 0 ? v : undefined })}
                        placeholder="Add enum value..."
                      />
                    </div>
                  )}
                </div>
              ))}
            </div>
            <button className="text-blue-400 hover:text-blue-300 text-xs mt-2" onClick={addField}>
              + Add Field
            </button>
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
