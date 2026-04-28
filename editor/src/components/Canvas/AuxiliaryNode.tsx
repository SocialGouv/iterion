import { Handle } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import type { EditingItem } from "@/store/ui";
import { useUIStore } from "@/store/ui";
import { LAYER_COLORS, LAYER_ICONS, SELECTED_BORDER, SELECTED_GLOW } from "@/lib/constants";
import type { LayerKind } from "@/lib/constants";
import { SIDES, POS_MAP } from "./handlePositions";

const LAYER_TO_ITEM_KIND: Record<LayerKind, EditingItem["kind"]> = {
  schemas: "schema",
  prompts: "prompt",
  vars: "var",
};

export interface AuxiliaryNodeData extends Record<string, unknown> {
  label: string;
  layerKind: LayerKind;
  subtitle: string;
  badge?: string;
}

export default function AuxiliaryNode({ data, selected }: NodeProps) {
  const { label, layerKind, subtitle, badge } = data as AuxiliaryNodeData;
  const color = LAYER_COLORS[layerKind];
  const icon = LAYER_ICONS[layerKind];
  const setEditingItem = useUIStore((s) => s.setEditingItem);

  return (
    <div
      className="rounded-full border px-3 py-1.5 min-w-[100px] text-center shadow-md cursor-pointer"
      style={{
        borderColor: selected ? SELECTED_BORDER : color,
        background: `${color}22`,
        boxShadow: selected ? SELECTED_GLOW : undefined,
      }}
      onClick={() => setEditingItem({ kind: LAYER_TO_ITEM_KIND[layerKind], name: label })}
      title="Click to edit"
    >
      {SIDES.map(s => (
        <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-surface-3 !w-1 !h-1 !opacity-0" />
      ))}
      <div className="flex items-center justify-center gap-1">
        <span className="text-xs">{icon}</span>
        <span className="font-medium text-xs text-fg-default truncate max-w-[90px]">{label}</span>
        {badge && (
          <span
            className="text-[8px] px-1 rounded-full text-fg-default/80"
            style={{ background: color + "44" }}
          >
            {badge}
          </span>
        )}
      </div>
      {subtitle && (
        <div className="text-[9px] text-fg-subtle truncate max-w-[110px]">{subtitle}</div>
      )}
      {SIDES.map(s => (
        <Handle key={`source-${s}`} id={`source-${s}`} type="source" position={POS_MAP[s]} className="!bg-surface-3 !w-1 !h-1 !opacity-0" />
      ))}
    </div>
  );
}
