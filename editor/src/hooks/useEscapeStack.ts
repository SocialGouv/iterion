import { useUIStore } from "@/store/ui";
import { useSelectionStore } from "@/store/selection";

/**
 * Returns an escape handler that dismisses UI layers in priority order.
 * Returns true if something was dismissed, false if nothing matched.
 *
 * Order:
 *   1. Editing item (schema/prompt/var single-edit view)
 *   2. Sub-node detail view (popped one level)
 *   3. Selection (node/edge)
 */
export function useEscapeStack(): () => boolean {
  const editingItem = useUIStore((s) => s.editingItem);
  const setEditingItem = useUIStore((s) => s.setEditingItem);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const popSubNodeView = useUIStore((s) => s.popSubNodeView);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);
  const clearSelection = useSelectionStore((s) => s.clearSelection);

  return () => {
    if (editingItem) { setEditingItem(null); return true; }
    if (subNodeViewStack.length > 0) { popSubNodeView(); return true; }
    if (selectedNodeId || selectedEdgeId) { clearSelection(); return true; }
    return false;
  };
}
