import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath } from "@xyflow/react";
import type { EdgeProps } from "@xyflow/react";
import { LAYER_COLORS } from "@/lib/constants";
import type { LayerKind } from "@/lib/constants";

export default function ReferenceEdge(props: EdgeProps) {
  const { sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, label, data, markerEnd } = props;

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
        markerEnd={markerEnd}
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
