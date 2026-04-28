import { useMemo } from "react";
import { useReactFlow } from "@xyflow/react";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";

export type InspectorMode =
  | { kind: "editing-item" }
  | { kind: "single-node"; nodeId: string }
  | { kind: "single-edge"; edgeId: string }
  | { kind: "multi"; nodeIds: string[]; edgeIds: string[] }
  | { kind: "empty" };

/**
 * Derives the Inspector's display mode from selection + edit-item state.
 *
 * Priority:
 *   1. editingItem (schema/prompt/var) -> single-item edit takes over.
 *   2. multi-select via XYFlow (more than one node/edge selected).
 *   3. single-node selection.
 *   4. single-edge selection.
 *   5. empty (default tabs).
 *
 * Must be used inside a ReactFlowProvider (always true in this app — App.tsx wraps the tree).
 */
export function useInspectorMode(): InspectorMode {
  const editingItem = useUIStore((s) => s.editingItem);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);
  const rf = useReactFlow();

  const multiNodeIds = rf.getNodes().filter((n) => n.selected).map((n) => n.id);
  const multiEdgeIds = rf.getEdges().filter((e) => e.selected).map((e) => e.id);
  const multiKey = `${multiNodeIds.join(",")}|${multiEdgeIds.join(",")}`;

  return useMemo<InspectorMode>(() => {
    if (editingItem) return { kind: "editing-item" };
    const totalSelected = multiNodeIds.length + multiEdgeIds.length;
    if (totalSelected > 1) {
      return { kind: "multi", nodeIds: multiNodeIds, edgeIds: multiEdgeIds };
    }
    if (selectedNodeId) return { kind: "single-node", nodeId: selectedNodeId };
    if (selectedEdgeId) return { kind: "single-edge", edgeId: selectedEdgeId };
    return { kind: "empty" };
    // multiKey captures changes in node/edge multi-selection arrays.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editingItem, selectedNodeId, selectedEdgeId, multiKey]);
}
