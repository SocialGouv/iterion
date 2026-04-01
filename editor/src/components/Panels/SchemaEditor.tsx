import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import type { SchemaDecl, SchemaField, FieldType } from "@/api/types";
import { defaultSchema } from "@/lib/defaults";
import { TextField, SelectField, TagListField } from "./forms/FormField";

const FIELD_TYPES: { value: FieldType; label: string }[] = [
  { value: "string", label: "string" },
  { value: "bool", label: "bool" },
  { value: "int", label: "int" },
  { value: "float", label: "float" },
  { value: "json", label: "json" },
  { value: "string[]", label: "string[]" },
];

export default function SchemaEditor() {
  const document = useDocumentStore((s) => s.document);
  const addSchema = useDocumentStore((s) => s.addSchema);
  const removeSchema = useDocumentStore((s) => s.removeSchema);
  const updateSchema = useDocumentStore((s) => s.updateSchema);

  const schemas = document?.schemas ?? [];

  const handleAdd = useCallback(() => {
    const existing = new Set(schemas.map((s) => s.name));
    let i = 1;
    while (existing.has(`schema_${i}`)) i++;
    addSchema(defaultSchema(`schema_${i}`));
  }, [schemas, addSchema]);

  return (
    <div className="p-3 text-sm">
      <div className="flex items-center justify-between mb-3">
        <h2 className="font-bold text-gray-300">Schemas</h2>
        <button
          className="bg-blue-600 hover:bg-blue-700 text-xs px-2 py-1 rounded"
          onClick={handleAdd}
          disabled={!document}
        >
          + New
        </button>
      </div>
      {schemas.length === 0 && <p className="text-gray-500 text-xs">No schemas defined.</p>}
      {schemas.map((schema) => (
        <SchemaCard key={schema.name} schema={schema} onUpdate={updateSchema} onRemove={removeSchema} />
      ))}
    </div>
  );
}

function SchemaCard({
  schema,
  onUpdate,
  onRemove,
}: {
  schema: SchemaDecl;
  onUpdate: (name: string, updates: Partial<SchemaDecl>) => void;
  onRemove: (name: string) => void;
}) {
  const updateField = useCallback(
    (index: number, updates: Partial<SchemaField>) => {
      const fields = schema.fields.map((f, i) => (i === index ? { ...f, ...updates } : f));
      onUpdate(schema.name, { fields });
    },
    [schema, onUpdate],
  );

  const addField = useCallback(() => {
    onUpdate(schema.name, { fields: [...schema.fields, { name: "", type: "string" as FieldType }] });
  }, [schema, onUpdate]);

  const removeField = useCallback(
    (index: number) => {
      onUpdate(schema.name, { fields: schema.fields.filter((_, i) => i !== index) });
    },
    [schema, onUpdate],
  );

  return (
    <div className="mb-4 p-2 bg-gray-800 rounded border border-gray-700">
      <div className="flex items-center justify-between mb-2">
        <TextField
          label="Schema Name"
          value={schema.name}
          onChange={(v) => onUpdate(schema.name, { name: v })}
        />
        <button className="text-red-400 hover:text-red-300 text-xs ml-2" onClick={() => onRemove(schema.name)}>
          Delete
        </button>
      </div>
      <div className="space-y-2">
        {schema.fields.map((field, i) => (
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
      <button className="text-blue-400 hover:text-blue-300 text-xs mt-1" onClick={addField}>
        + Add Field
      </button>
    </div>
  );
}
