import { useCallback, useState } from "react";
import { useDocumentStore } from "@/store/document";
import type { SchemaDecl, SchemaField, FieldType } from "@/api/types";
import { defaultSchema } from "@/lib/defaults";
import { TextField, CommittedTextField, SelectField, TagListField } from "./forms/FormField";
import ConfirmDialog from "../shared/ConfirmDialog";

const FIELD_TYPES: { value: FieldType; label: string }[] = [
  { value: "string", label: "string" },
  { value: "bool", label: "bool" },
  { value: "int", label: "int" },
  { value: "float", label: "float" },
  { value: "json", label: "json" },
  { value: "string[]", label: "string[]" },
];

interface SchemaEditorProps {
  /** When set, renders only that schema's card (used by the Inspector "edit item" mode). */
  filterName?: string;
}

export default function SchemaEditor({ filterName }: SchemaEditorProps = {}) {
  const document = useDocumentStore((s) => s.document);
  const addSchema = useDocumentStore((s) => s.addSchema);
  const removeSchema = useDocumentStore((s) => s.removeSchema);
  const updateSchema = useDocumentStore((s) => s.updateSchema);
  const renameSchema = useDocumentStore((s) => s.renameSchema);

  const schemas = document?.schemas ?? [];
  const visible = filterName ? schemas.filter((s) => s.name === filterName) : schemas;

  const handleAdd = useCallback(() => {
    const existing = new Set(schemas.map((s) => s.name));
    let i = 1;
    while (existing.has(`schema_${i}`)) i++;
    addSchema(defaultSchema(`schema_${i}`));
  }, [schemas, addSchema]);

  return (
    <div className="p-3 text-sm">
      {!filterName && (
        <div className="flex items-center justify-between mb-3">
          <h2 className="font-bold text-fg-muted">Schemas</h2>
          <button
            className="bg-accent hover:bg-accent-hover text-xs px-2 py-1 rounded"
            onClick={handleAdd}
            disabled={!document}
          >
            + New
          </button>
        </div>
      )}
      {visible.length === 0 && (
        <p className="text-fg-subtle text-xs">
          {filterName ? `Schema "${filterName}" not found.` : "No schemas defined."}
        </p>
      )}
      {visible.map((schema) => (
        <SchemaCard key={schema.name} schema={schema} allSchemas={schemas} onUpdate={updateSchema} onRemove={removeSchema} onRename={renameSchema} />
      ))}
    </div>
  );
}

function SchemaCard({
  schema,
  allSchemas,
  onUpdate,
  onRemove,
  onRename,
}: {
  schema: SchemaDecl;
  allSchemas: SchemaDecl[];
  onUpdate: (name: string, updates: Partial<SchemaDecl>) => void;
  onRemove: (name: string) => void;
  onRename: (oldName: string, newName: string) => void;
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

  const [confirmDelete, setConfirmDelete] = useState(false);

  return (
    <div className="mb-4 p-2 bg-surface-1 rounded border border-border-default">
      <div className="flex items-center justify-between mb-2">
        <CommittedTextField
          label="Schema Name"
          value={schema.name}
          onChange={(v) => onRename(schema.name, v)}
          validate={(v) => {
            if (!v.trim()) return "Name cannot be empty";
            const names = new Set(allSchemas.map((s) => s.name));
            names.delete(schema.name);
            if (names.has(v)) return "Schema name already exists";
            return null;
          }}
        />
        <button className="text-danger hover:text-danger-fg text-xs ml-2" onClick={() => setConfirmDelete(true)}>
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
              <button className="text-danger hover:text-danger-fg text-xs pb-2" onClick={() => removeField(i)}>
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
      <button className="text-accent hover:text-accent text-xs mt-1" onClick={addField}>
        + Add Field
      </button>
      <ConfirmDialog
        open={confirmDelete}
        title="Delete Schema"
        message={`Delete schema "${schema.name}"? Nodes referencing it will lose their schema assignment.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={() => { onRemove(schema.name); setConfirmDelete(false); }}
        onCancel={() => setConfirmDelete(false)}
      />
    </div>
  );
}
