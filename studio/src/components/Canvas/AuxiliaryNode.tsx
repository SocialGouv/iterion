import { Handle } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import type { EditingItem } from "@/store/ui";
import { useUIStore } from "@/store/ui";
import { clickableRowProps } from "@/lib/a11y";
import { LAYER_COLORS, LAYER_ICONS, SELECTED_BORDER, SELECTED_GLOW, softColor } from "@/lib/constants";
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

  const openEditor = () => setEditingItem({ kind: LAYER_TO_ITEM_KIND[layerKind], name: label });
  return (
    <div
      {...clickableRowProps(openEditor)}
      className="rounded-full border px-3 py-1.5 min-w-[100px] text-center shadow-[var(--shadow-sm)] cursor-pointer focus:outline-none focus:ring-2 focus:ring-accent"
      style={{
        borderColor: selected ? SELECTED_BORDER : color,
        background: softColor(color),
        boxShadow: selected ? SELECTED_GLOW : undefined,
      }}
      title="Click to edit"
    >
      {SIDES.map(s => (
        <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-surface-3 !w-1 !h-1 !opacity-0" />
      ))}
      <div className="flex items-center justify-center gap-1">
        <span className="text-xs">{icon}</span>
        <span className="font-medium text-xs text-fg-default truncate max-w-[90px]" title={label}>{label}</span>
        {badge && (
          <span
            className="text-[8px] px-1 rounded-full text-fg-default"
            style={{ background: softColor(color, 27) }}
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
