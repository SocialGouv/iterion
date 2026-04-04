import { useUIStore } from "@/store/ui";
import EditPromptModal from "./EditPromptModal";
import EditSchemaModal from "./EditSchemaModal";
import EditVarModal from "./EditVarModal";

export default function EditItemModal() {
  const editingItem = useUIStore((s) => s.editingItem);
  if (!editingItem) return null;

  switch (editingItem.kind) {
    case "prompt": return <EditPromptModal name={editingItem.name} />;
    case "schema": return <EditSchemaModal name={editingItem.name} />;
    case "var":    return <EditVarModal name={editingItem.name} />;
  }
}
