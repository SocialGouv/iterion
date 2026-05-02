import { useUIStore } from "@/store/ui";
import SchemaEditor from "@/components/Panels/SchemaEditor";
import PromptEditor from "@/components/Panels/PromptEditor";
import VarsEditor from "@/components/Panels/VarsEditor";
import { Button } from "@/components/ui";
import { ArrowLeftIcon } from "@radix-ui/react-icons";

const KIND_LABEL = {
  schema: "Schema",
  prompt: "Prompt",
  var: "Variable",
};

/**
 * Single-item edit mode: focuses the Inspector on one schema / prompt / var.
 * Reuses the existing tab editors via their `filterName` prop, avoiding
 * duplicate form code.
 */
export default function InspectorEditItem() {
  const editingItem = useUIStore((s) => s.editingItem);
  const setEditingItem = useUIStore((s) => s.setEditingItem);

  if (!editingItem) return null;

  const { kind, name } = editingItem;

  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center gap-2 border-b border-border-default px-3 py-2 shrink-0">
        <Button
          variant="ghost"
          size="sm"
          leadingIcon={<ArrowLeftIcon />}
          onClick={() => setEditingItem(null)}
        >
          Back
        </Button>
        <div className="text-xs text-fg-subtle">
          {KIND_LABEL[kind]} <span className="text-fg-default">/ {name}</span>
        </div>
      </div>
      <div className="flex-1 overflow-y-auto">
        {kind === "schema" && <SchemaEditor filterName={name} />}
        {kind === "prompt" && <PromptEditor filterName={name} compact={false} />}
        {kind === "var" && <VarsEditor filterName={name} />}
      </div>
    </div>
  );
}
