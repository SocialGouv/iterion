import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath } from "@xyflow/react";
import type { EdgeProps } from "@xyflow/react";
import type { LayerKind } from "@/store/ui";

const LAYER_COLORS: Record<LayerKind, string> = {
  schemas: "#A78BFA",
  prompts: "#2DD4BF",
  vars: "#FBBF24",
};

export default function ReferenceEdge(props: EdgeProps) {
  const { sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, label, data } = props;

  const layerKind = (data as Record<string, unknown>)?.layerKind as LayerKind | undefined;
  const color = layerKind ? LAYER_COLORS[layerKind] : "#666";

  const [edgePath, labelX, labelY] = getSmoothStepPath({
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    borderRadius: 6,
    offset: 15,
  });

  return (
    <>
      <BaseEdge
        path={edgePath}
        style={{
          stroke: color,
          strokeDasharray: "4 3",
          strokeWidth: 1,
          opacity: 0.6,
        }}
      />
      {label && (
        <EdgeLabelRenderer>
          <div
            className="absolute text-[9px] px-1 py-0.5 rounded pointer-events-none whitespace-nowrap"
            style={{
              transform: `translate(-50%, -50%) translate(${labelX}px,${labelY}px)`,
              color,
              opacity: 0.8,
            }}
          >
            {label}
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}
