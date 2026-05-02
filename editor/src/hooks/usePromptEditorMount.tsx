import { useCallback, useState } from "react";
import { useDocumentStore } from "@/store/document";
import PromptEditorModal from "@/components/Panels/PromptEditorModal";

/**
 * Shared prompt-editor wiring used by every node form that exposes a
 * `PromptPickerField`. Centralises the modal state + body lookup so
 * the contract lives in one place instead of being copy-pasted into
 * AgentForm / HumanForm / RouterForm.
 *
 * Usage:
 *   const { openPromptEditor, lookupBody, promptModal } = usePromptEditorMount();
 *   ...
 *   <PromptPickerField onEdit={openPromptEditor} body={lookupBody(decl.system)} ... />
 *   {promptModal}
 */
export function usePromptEditorMount() {
  const prompts = useDocumentStore((s) => s.document?.prompts);
  const [editingPrompt, setEditingPrompt] = useState<string | null>(null);

  const lookupBody = useCallback(
    (name: string | undefined): string => {
      if (!name) return "";
      return (prompts ?? []).find((p) => p.name === name)?.body ?? "";
    },
    [prompts],
  );

  const promptModal = (
    <PromptEditorModal
      open={editingPrompt !== null}
      onOpenChange={(open) => {
        if (!open) setEditingPrompt(null);
      }}
      promptName={editingPrompt ?? ""}
    />
  );

  return {
    openPromptEditor: setEditingPrompt as (name: string) => void,
    lookupBody,
    promptModal,
  };
}
