import { Handle, Position } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import type { LayerKind, EditingItem } from "@/store/ui";
import { useUIStore } from "@/store/ui";

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
  const layoutDirection = useUIStore((s) => s.layoutDirection);

  const targetPos = layoutDirection === "RIGHT" ? Position.Left : Position.Top;
  const sourcePos = layoutDirection === "RIGHT" ? Position.Right : Position.Bottom;

  return (
    <div
      className="rounded-full border px-3 py-1.5 min-w-[100px] text-center shadow-md cursor-pointer"
      style={{ borderColor: style.color, background: style.bg }}
      onClick={() => setEditingItem({ kind: LAYER_TO_ITEM_KIND[layerKind], name: label })}
      title="Click to edit"
    >
      <Handle type="target" position={targetPos} className="!bg-gray-500 !w-1.5 !h-1.5" />
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
      <Handle type="source" position={sourcePos} className="!bg-gray-500 !w-1.5 !h-1.5" />
    </div>
  );
}
