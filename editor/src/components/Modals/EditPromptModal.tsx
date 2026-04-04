import { useState, useEffect } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { TextField } from "@/components/Panels/forms/FormField";

export default function EditPromptModal({ name }: { name: string }) {
  const document = useDocumentStore((s) => s.document);
  const updatePrompt = useDocumentStore((s) => s.updatePrompt);
  const renamePrompt = useDocumentStore((s) => s.renamePrompt);
  const setEditingItem = useUIStore((s) => s.setEditingItem);

  const prompt = document?.prompts?.find((p) => p.name === name);
  const allPromptNames = document?.prompts?.map((p) => p.name) ?? [];

  // Local draft state
  const [draftName, setDraftName] = useState(name);
  const [draftBody, setDraftBody] = useState(prompt?.body ?? "");

  // Reset draft when the source prompt changes (e.g. external edit)
  useEffect(() => {
    if (prompt) {
      setDraftName(prompt.name);
      setDraftBody(prompt.body);
    }
  }, [prompt]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") setEditingItem(null);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [setEditingItem]);

  if (!prompt) return null;

  // Validation
  const nameError = (() => {
    if (!draftName.trim()) return "Name cannot be empty";
    const others = new Set(allPromptNames);
    others.delete(name);
    if (others.has(draftName)) return "Prompt name already exists";
    return null;
  })();

  const hasChanges = draftName !== name || draftBody !== prompt.body;
  const canSave = hasChanges && !nameError;

  const handleSave = () => {
    if (!canSave) return;
    if (draftName !== name) renamePrompt(name, draftName);
    if (draftBody !== prompt.body) updatePrompt(draftName, { body: draftBody });
    setEditingItem(null);
  };

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" onClick={() => setEditingItem(null)}>
      <div
        className="bg-gray-800 border border-gray-600 rounded-lg p-4 w-[500px] max-h-[80vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between mb-3">
          <h3 className="text-sm font-bold text-white">Edit Prompt</h3>
          <button className="text-gray-400 hover:text-white text-lg px-1" onClick={() => setEditingItem(null)}>&times;</button>
        </div>
        <div className="space-y-3">
          <div>
            <TextField
              label="Name"
              value={draftName}
              onChange={setDraftName}
              placeholder="prompt_name"
            />
            {nameError && <div className="text-[10px] text-red-400 mt-0.5">{nameError}</div>}
          </div>
          <TextField
            label="Body"
            value={draftBody}
            onChange={setDraftBody}
            multiline
            rows={12}
            placeholder="Prompt template... use {{vars.x}} or {{outputs.node.field}}"
          />
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
