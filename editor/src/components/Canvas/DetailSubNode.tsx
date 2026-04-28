import { Handle } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import { useUIStore } from "@/store/ui";
import { useSelectionStore } from "@/store/selection";
import { makeEdgeId } from "@/lib/documentToGraph";
import { SELECTED_BORDER, SELECTED_GLOW, SUB_COLORS, SUB_ICONS } from "@/lib/constants";
import type { DetailSubKind } from "@/lib/constants";
import { SIDES, POS_MAP } from "./handlePositions";

export type { DetailSubKind };

export interface DetailSubNodeData extends Record<string, unknown> {
  subKind: DetailSubKind;
  label: string;
  subtitle?: string;
  badge?: string;
  relation?: string;
  /** For schema/prompt/var: the item name for editing */
  itemName?: string;
  /** For edge sub-nodes: edge index and workflow */
  edgeIndex?: number;
  workflowName?: string;
  /** For edge sub-nodes: the remote node (for navigation) */
  targetNodeId?: string;
}


export default function DetailSubNode({ data, selected }: NodeProps) {
  const { subKind, label, subtitle, badge, itemName, edgeIndex, workflowName } = data as DetailSubNodeData;
  const color = SUB_COLORS[subKind];
  const icon = SUB_ICONS[subKind];
  const setEditingItem = useUIStore((s) => s.setEditingItem);
  const clearSubNodeView = useUIStore((s) => s.clearSubNodeView);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);

  const handleClick = () => {
    if (subKind === "schema" && itemName) {
      setEditingItem({ kind: "schema", name: itemName });
    } else if (subKind === "prompt" && itemName) {
      setEditingItem({ kind: "prompt", name: itemName });
    } else if (subKind === "var" && itemName) {
      setEditingItem({ kind: "var", name: itemName });
    } else if (subKind === "edge" && workflowName != null && edgeIndex != null) {
      // Pop back to the global view and select the edge so the Inspector
      // shows the EdgeForm for it.
      setSelectedEdge(makeEdgeId(workflowName, edgeIndex));
      clearSubNodeView();
    }
  };

  return (
    <div
      className="rounded-lg border px-3 py-2 min-w-[130px] max-w-[200px] text-center shadow-md cursor-pointer hover:brightness-125 transition-all"
      style={{
        borderColor: selected ? SELECTED_BORDER : color,
        background: `${color}18`,
        boxShadow: selected ? SELECTED_GLOW : undefined,
      }}
      onClick={handleClick}
      title={`Click to edit ${subKind}`}
    >
      {SIDES.map((s) => (
        <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-surface-3 !w-1 !h-1 !opacity-0" />
      ))}
      <div className="flex items-center justify-center gap-1.5">
        <span className="text-xs">{icon}</span>
        <span className="font-medium text-xs text-fg-default truncate max-w-[120px]">{label}</span>
        {badge && (
          <span
            className="text-[8px] px-1.5 py-0.5 rounded-full text-fg-default/80"
            style={{ background: color + "44" }}
          >
            {badge}
          </span>
        )}
      </div>
      {subtitle && (
        <div className="text-[9px] text-fg-subtle mt-0.5 truncate max-w-[180px]">{subtitle}</div>
      )}
      {SIDES.map((s) => (
        <Handle key={`source-${s}`} id={`source-${s}`} type="source" position={POS_MAP[s]} className="!bg-surface-3 !w-1 !h-1 !opacity-0" />
      ))}
    </div>
  );
}
