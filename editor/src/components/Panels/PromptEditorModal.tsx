import { Dialog } from "@/components/ui/Dialog";
import PromptEditor from "./PromptEditor";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  promptName: string;
}

/**
 * Full-size modal wrapper around the existing `PromptEditor`. Reuses
 * the prompt card (rename/delete/body) via its `filterName` prop and
 * unlocks `compact={false}` so the body is rendered with the
 * monospace highlighter editor instead of the cramped 6-row textarea
 * used in side-panel context.
 *
 * Note: Radix Dialog already handles ESC + overlay click to close,
 * so this modal does NOT register itself with `useEscapeStack` —
 * doing so would double-handle the key.
 */
export default function PromptEditorModal({ open, onOpenChange, promptName }: Props) {
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={
        <span className="flex items-center gap-2">
          <span className="text-fg-subtle">Prompt</span>
          <span className="font-mono text-fg-default">{promptName}</span>
        </span>
      }
      description="Edit prompt body. Use {{...}} for template references and ${VAR} for env vars."
      widthClass="max-w-3xl"
    >
      <div className="h-[70vh] overflow-y-auto -mx-4">
        <PromptEditor filterName={promptName} compact={false} />
      </div>
    </Dialog>
  );
}
