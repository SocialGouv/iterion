import type { Node, NodeProps } from "@xyflow/react";

// Synthetic, non-interactive background node that frames the replicated
// region of a fan_out_each router on the RUN canvas. It is NOT a ReactFlow
// parent node (the ELK layout already positions the real nodes in absolute
// coords); it is sized to the bounding box of the region's nodes and drawn
// behind them (zIndex < 0, pointer-events: none) so clicks still reach the
// real nodes. Built in RunCanvasIR after layout settles.
//
// Purpose: make it obvious that everything inside the frame executes ONCE
// PER ITEM (the per-item body), while nodes outside (start/router/join/done)
// run a single time — the thing that's otherwise unreadable from per-node
// iteration pips alone.
export interface FanoutFrameData extends Record<string, unknown> {
  width: number;
  height: number;
  // e.g. "fan_out_each · dispatch"
  label: string;
  // Number of items the router fanned out over (e.g. 38). Known only in the
  // run canvas; undefined in the editor (no runtime), where the header omits
  // the count.
  total?: number;
}

type FanoutFrameType = Node<FanoutFrameData, "fanoutFrame">;

export default function FanoutFrame({ data }: NodeProps<FanoutFrameType>) {
  const { width, height, label, total } = data;
  return (
    <div
      className="relative rounded-2xl border-2 border-dashed border-info/50"
      style={{
        width,
        height,
        pointerEvents: "none",
        background: "color-mix(in srgb, var(--color-info) 6%, transparent)",
      }}
    >
      <div
        className="absolute -top-2.5 left-3 px-2 py-0.5 rounded bg-info-soft text-info-fg text-[10px] font-medium border border-info/50 whitespace-nowrap shadow-sm"
        style={{ pointerEvents: "none" }}
        title={
          total != null
            ? `Replicated region: every node inside runs once per item (${total} items). ‖ = multi-instance.`
            : `Replicated region: every node inside runs once per item of the fan_out_each array. ‖ = multi-instance.`
        }
      >
        ⛓ {label}
        {total != null ? ` · ×${total} per item` : " · per item"} · ‖ multi-instance
      </div>
    </div>
  );
}
