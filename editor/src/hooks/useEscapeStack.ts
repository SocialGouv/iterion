import { useUIStore } from "@/store/ui";

/**
 * Returns an escape handler that dismisses UI layers in priority order.
 * Returns true if something was dismissed, false if nothing matched.
 */
export function useEscapeStack(): () => boolean {
  const editModalEdgeInfo = useUIStore((s) => s.editModalEdgeInfo);
  const setEditModalEdgeInfo = useUIStore((s) => s.setEditModalEdgeInfo);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const popSubNodeView = useUIStore((s) => s.popSubNodeView);

  return () => {
    if (editModalEdgeInfo) { setEditModalEdgeInfo(null); return true; }
    if (subNodeViewStack.length > 0) { popSubNodeView(); return true; }
    return false;
  };
}
