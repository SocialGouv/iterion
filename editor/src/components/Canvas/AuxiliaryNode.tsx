import { Handle } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import type { LayerKind, EditingItem } from "@/store/ui";
import { useUIStore } from "@/store/ui";
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

const LAYER_STYLES: Record<LayerKind, { color: string; bg: string; icon: string }> = {
  schemas: { color: "#A78BFA", bg: "#A78BFA22", icon: "\u{1F4D0}" },
  prompts: { color: "#2DD4BF", bg: "#2DD4BF22", icon: "\u{1F4DD}" },
  vars:    { color: "#FBBF24", bg: "#FBBF2422", icon: "\u{1F3F7}\u{FE0F}" },
};

export default function AuxiliaryNode({ data }: NodeProps) {
  const { label, layerKind, subtitle, badge } = data as AuxiliaryNodeData;
  const style = LAYER_STYLES[layerKind];
  const setEditingItem = useUIStore((s) => s.setEditingItem);

  return (
    <div
      className="rounded-full border px-3 py-1.5 min-w-[100px] text-center shadow-md cursor-pointer"
      style={{ borderColor: style.color, background: style.bg }}
      onClick={() => setEditingItem({ kind: LAYER_TO_ITEM_KIND[layerKind], name: label })}
      title="Click to edit"
    >
      {SIDES.map(s => (
        <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-gray-500 !w-1 !h-1 !opacity-0" />
      ))}
      <div className="flex items-center justify-center gap-1">
        <span className="text-xs">{style.icon}</span>
        <span className="font-medium text-xs text-white truncate max-w-[90px]">{label}</span>
        {badge && (
          <span
            className="text-[8px] px-1 rounded-full text-white/80"
            style={{ background: style.color + "44" }}
          >
            {badge}
          </span>
        )}
      </div>
      {subtitle && (
        <div className="text-[9px] text-gray-400 truncate max-w-[110px]">{subtitle}</div>
      )}
      {SIDES.map(s => (
        <Handle key={`source-${s}`} id={`source-${s}`} type="source" position={POS_MAP[s]} className="!bg-gray-500 !w-1 !h-1 !opacity-0" />
      ))}
    </div>
  );
}
