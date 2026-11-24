import { useCallback, useId, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { defaultPrompt } from "@/lib/defaults";
import { TextField, CommittedTextField } from "./forms/FormField";
import PromptBodyEditor from "./PromptBodyEditor";
import ConfirmDialog from "../shared/ConfirmDialog";
import { Button } from "@/components/ui/Button";

interface PromptEditorProps {
  /** When set, renders only that prompt's card (used by the Inspector "edit item" mode). */
  filterName?: string;
  /**
   * Compact mode renders the body as the historical 6-row textarea —
   * appropriate for the cramped sidebar tab. When false, the body is
   * rendered with the prompt-first highlighter editor (used by the
   * full-size modal and the single-item edit pane).
   */
  compact?: boolean;
}

export default function PromptEditor({ filterName, compact = true }: PromptEditorProps = {}) {
  const document = useDocumentStore((s) => s.document);
  const addPrompt = useDocumentStore((s) => s.addPrompt);
  const removePrompt = useDocumentStore((s) => s.removePrompt);
  const updatePrompt = useDocumentStore((s) => s.updatePrompt);
  const renamePrompt = useDocumentStore((s) => s.renamePrompt);

  const prompts = document?.prompts ?? [];
  const visible = filterName ? prompts.filter((p) => p.name === filterName) : prompts;

  const handleAdd = useCallback(() => {
    const existing = new Set(prompts.map((p) => p.name));
    let i = 1;
    while (existing.has(`prompt_${i}`)) i++;
    addPrompt(defaultPrompt(`prompt_${i}`));
  }, [prompts, addPrompt]);

  return (
    <div className="p-3 text-sm">
      {!filterName && (
        <div className="flex items-center justify-between mb-3">
          <h2 className="font-bold text-fg-muted">Prompts</h2>
          <Button
            variant="primary"
            size="sm"
            onClick={handleAdd}
            disabled={!document}
          >
            + New
          </Button>
        </div>
      )}
      {visible.length === 0 && (
        <p className="text-fg-subtle text-xs">
          {filterName ? `Prompt "${filterName}" not found.` : "No prompts defined."}
        </p>
      )}
      {visible.map((prompt, i) => (
        <PromptCard
          key={prompt.name}
          name={prompt.name}
          body={prompt.body}
          index={i}
          allPromptNames={prompts.map((p) => p.name)}
          compact={compact}
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
  index,
  allPromptNames,
  compact,
  onRename,
  onUpdateBody,
  onRemove,
}: {
  name: string;
  body: string;
  /** 1-based fallback in the delete button's name while the name is empty. */
  index: number;
  allPromptNames: string[];
  compact: boolean;
  onRename: (v: string) => void;
  onUpdateBody: (v: string) => void;
  onRemove: () => void;
}) {
  const [confirmDelete, setConfirmDelete] = useState(false);
  // One card renders one body editor; the visible caption labels it.
  const bodyId = useId();

  return (
    <div className="mb-4 p-2 bg-surface-1 rounded border border-border-default">
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
        {/* Sibling cards repeat the visible "Delete"; the prompt name is
            what a screen reader needs to tell them apart. */}
        <Button
          variant="ghost"
          size="sm"
          className="text-danger hover:text-danger-fg ml-2"
          aria-label={`Delete prompt ${name || index + 1}`}
          onClick={() => setConfirmDelete(true)}
        >
          Delete
        </Button>
      </div>
      {compact ? (
        <TextField
          label="Body"
          value={body}
          onChange={onUpdateBody}
          multiline
          rows={6}
          placeholder="Prompt template... use {{vars.x}} or {{outputs.node.field}}"
          refContext={{ kind: "prompt-body" }}
        />
      ) : (
        <div className="mb-2">
          <label htmlFor={bodyId} className="block text-xs text-fg-subtle mb-1">
            Prompt body
          </label>
          <PromptBodyEditor
            value={body}
            onChange={onUpdateBody}
            rows={20}
            placeholder="Prompt template... use {{vars.x}} or {{outputs.node.field}}"
            refContext={{ kind: "prompt-body" }}
            id={bodyId}
          />
        </div>
      )}
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
