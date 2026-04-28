import { Handle } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import { useGroupStore } from "@/store/groups";
import { groupNameFromNodeId } from "@/lib/groups";
import { SELECTED_BORDER, SELECTED_GLOW } from "@/lib/constants";
import { SIDES, POS_MAP } from "./handlePositions";

export interface GroupNodeData extends Record<string, unknown> {
  groupName: string;
  nodeCount: number;
  childKinds: string[];
  color: string;
}

const GROUP_COLOR = "#6366F1"; // indigo

export default function GroupNode({ id, data, selected }: NodeProps) {
  const { groupName, nodeCount, childKinds, color } = data as unknown as GroupNodeData;
  const name = groupName ?? groupNameFromNodeId(id);
  const collapsed = useGroupStore((s) => s.collapsedGroups.has(name));
  const toggleCollapse = useGroupStore((s) => s.toggleCollapse);
  const themeColor = color || GROUP_COLOR;

  if (collapsed) {
    // Collapsed view: compact summary node
    return (
      <div
        className="rounded-lg border-2 px-4 py-3 min-w-[160px] text-center shadow-lg cursor-pointer"
        style={{
          borderColor: selected ? SELECTED_BORDER : themeColor,
          background: `${themeColor}22`,
          borderStyle: "dashed",
          boxShadow: selected ? SELECTED_GLOW : undefined,
        }}
        onDoubleClick={(e) => { e.stopPropagation(); toggleCollapse(name); }}
      >
        {SIDES.map(s => (
          <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
        ))}
        <div className="flex items-center justify-center gap-1.5">
          <span className="text-lg">{"\u{1F4E6}"}</span>
          <span className="font-semibold text-sm text-fg-default">{name}</span>
        </div>
        <div className="text-xs text-fg-subtle mt-0.5">
          {nodeCount} node{nodeCount !== 1 ? "s" : ""}
        </div>
        {childKinds.length > 0 && (
          <div className="flex items-center justify-center gap-1 mt-1 flex-wrap">
            {childKinds.map((k, i) => (
              <span key={i} className="text-[9px] bg-surface-2/60 text-fg-muted px-1 rounded">{k}</span>
            ))}
          </div>
        )}
        <div className="text-[9px] text-fg-subtle mt-1">double-click to expand</div>
        {SIDES.map(s => (
          <Handle key={`source-${s}`} id={`source-${s}`} type="source" position={POS_MAP[s]} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
        ))}
      </div>
    );
  }

  // Expanded view: container background
  return (
    <div
      className="rounded-xl border-2 w-full h-full"
      style={{
        borderColor: selected ? SELECTED_BORDER : `${themeColor}66`,
        background: `${themeColor}0A`,
        borderStyle: "dashed",
        boxShadow: selected ? SELECTED_GLOW : undefined,
        minWidth: "100%",
        minHeight: "100%",
      }}
    >
      {/* Group header */}
      <div
        className="flex items-center gap-1.5 px-3 py-1.5 cursor-pointer select-none"
        style={{ borderBottom: `1px solid ${themeColor}33` }}
        onDoubleClick={(e) => { e.stopPropagation(); toggleCollapse(name); }}
      >
        <span className="text-xs">{"\u{1F4E6}"}</span>
        <span className="text-xs font-medium text-fg-muted">{name}</span>
        <span className="text-[9px] text-fg-subtle ml-auto">{nodeCount} nodes</span>
      </div>
    </div>
  );
}
