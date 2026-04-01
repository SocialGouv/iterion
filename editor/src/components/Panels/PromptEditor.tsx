import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { defaultPrompt } from "@/lib/defaults";
import { TextField } from "./forms/FormField";

export default function PromptEditor() {
  const document = useDocumentStore((s) => s.document);
  const addPrompt = useDocumentStore((s) => s.addPrompt);
  const removePrompt = useDocumentStore((s) => s.removePrompt);
  const updatePrompt = useDocumentStore((s) => s.updatePrompt);

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
        <div key={prompt.name} className="mb-4 p-2 bg-gray-800 rounded border border-gray-700">
          <div className="flex items-center justify-between mb-1">
            <TextField
              label="Prompt Name"
              value={prompt.name}
              onChange={(v) => updatePrompt(prompt.name, { name: v })}
            />
            <button className="text-red-400 hover:text-red-300 text-xs ml-2" onClick={() => removePrompt(prompt.name)}>
              Delete
            </button>
          </div>
          <TextField
            label="Body"
            value={prompt.body}
            onChange={(v) => updatePrompt(prompt.name, { body: v })}
            multiline
            rows={6}
            placeholder="Prompt template... use {{vars.x}} or {{outputs.node.field}}"
          />
        </div>
      ))}
    </div>
  );
}
