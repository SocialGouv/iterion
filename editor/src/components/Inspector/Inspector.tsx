import { useInspectorMode } from "@/hooks/useInspectorMode";
import InspectorEmpty from "./InspectorEmpty";
import InspectorEditItem from "./InspectorEditItem";
import InspectorNode from "./InspectorNode";
import InspectorEdge from "./InspectorEdge";
import InspectorMulti from "./InspectorMulti";

/**
 * Right-side editing surface. Dispatches between empty (default tabs),
 * single-node, single-edge, multi-select, and editing-item modes based on
 * selection + UI state.
 */
export default function Inspector() {
  const mode = useInspectorMode();

  switch (mode.kind) {
    case "editing-item":
      return <InspectorEditItem />;
    case "single-node":
      return <InspectorNode nodeId={mode.nodeId} />;
    case "single-edge":
      return <InspectorEdge edgeId={mode.edgeId} />;
    case "multi":
      return <InspectorMulti nodeIds={mode.nodeIds} edgeIds={mode.edgeIds} />;
    case "empty":
    default:
      return <InspectorEmpty />;
  }
}
