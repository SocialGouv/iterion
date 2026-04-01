import { useCallback, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { defaultPrompt } from "@/lib/defaults";
import { TextField, CommittedTextField } from "./forms/FormField";
import ConfirmDialog from "../shared/ConfirmDialog";

export default function PromptEditor() {
  const document = useDocumentStore((s) => s.document);
  const addPrompt = useDocumentStore((s) => s.addPrompt);
  const removePrompt = useDocumentStore((s) => s.removePrompt);
  const updatePrompt = useDocumentStore((s) => s.updatePrompt);
  const renamePrompt = useDocumentStore((s) => s.renamePrompt);

  const prompts = document?.prompts ?? [];

  const handleAdd = useCallback(() => {
    const existing = new Set(prompts.map((p) => p.name));
    let i = 1;
    while (existing.has(`prompt_${i}`)) i++;
    addPrompt(defaultPrompt(`prompt_${i}`));
  }, [prompts, addPrompt]);

  return (
    <div className="p-3 text-sm">
      <div className="flex items-center justify-between mb-3">
        <h2 className="font-bold text-gray-300">Prompts</h2>
        <button
          className="bg-blue-600 hover:bg-blue-700 text-xs px-2 py-1 rounded"
          onClick={handleAdd}
          disabled={!document}
        >
          + New
        </button>
      </div>
      {prompts.length === 0 && <p className="text-gray-500 text-xs">No prompts defined.</p>}
      {prompts.map((prompt) => (
        <PromptCard
          key={prompt.name}
          name={prompt.name}
          body={prompt.body}
          allPromptNames={prompts.map((p) => p.name)}
          onRename={(v) => renamePrompt(prompt.name, v)}
          onUpdateBody={(v) => updatePrompt(prompt.name, { body: v })}
          onRemove={() => removePrompt(prompt.name)}
        />
      ))}
    </div>
  );
}

function PromptCard({
  name,
  body,
  allPromptNames,
  onRename,
  onUpdateBody,
  onRemove,
}: {
  name: string;
  body: string;
  allPromptNames: string[];
  onRename: (v: string) => void;
  onUpdateBody: (v: string) => void;
  onRemove: () => void;
}) {
  const [confirmDelete, setConfirmDelete] = useState(false);

  return (
    <div className="mb-4 p-2 bg-gray-800 rounded border border-gray-700">
      <div className="flex items-center justify-between mb-1">
        <CommittedTextField
          label="Prompt Name"
          value={name}
          onChange={onRename}
          validate={(v) => {
            if (!v.trim()) return "Name cannot be empty";
            const others = new Set(allPromptNames);
            others.delete(name);
            if (others.has(v)) return "Prompt name already exists";
            return null;
          }}
        />
        <button className="text-red-400 hover:text-red-300 text-xs ml-2" onClick={() => setConfirmDelete(true)}>
          Delete
        </button>
      </div>
      <TextField
        label="Body"
        value={body}
        onChange={onUpdateBody}
        multiline
        rows={6}
        placeholder="Prompt template... use {{vars.x}} or {{outputs.node.field}}"
      />
      <ConfirmDialog
        open={confirmDelete}
        title="Delete Prompt"
        message={`Delete prompt "${name}"? Nodes referencing it will lose their prompt assignment.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={() => { onRemove(); setConfirmDelete(false); }}
        onCancel={() => setConfirmDelete(false)}
      />
    </div>
  );
}
