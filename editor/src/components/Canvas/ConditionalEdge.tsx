import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath } from "@xyflow/react";
import type { EdgeProps } from "@xyflow/react";

export default function ConditionalEdge(props: EdgeProps) {
  const { sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, label, data, selected } = props;

  const hasLoop = !!(data as Record<string, unknown>)?.loop;

  const [edgePath, labelX, labelY] = getSmoothStepPath({
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    borderRadius: hasLoop ? 16 : 8,
    offset: hasLoop ? 40 : 20,
  });

  const strokeColor = selected ? "#60A5FA" : hasLoop ? "#F59E0B" : "#888";
  const strokeDasharray = hasLoop ? "8 4" : undefined;
  const strokeWidth = selected ? 3 : hasLoop ? 2.5 : 1;

  return (
    <>
      <BaseEdge
        path={edgePath}
        style={{
          stroke: strokeColor,
          strokeDasharray,
          strokeWidth,
          filter: selected ? "drop-shadow(0 0 4px rgba(96, 165, 250, 0.6))" : undefined,
          animation: hasLoop ? "dash-flow 1s linear infinite" : undefined,
        }}
      />
      {label && (
        <EdgeLabelRenderer>
          <div
            className="absolute text-xs px-1.5 py-0.5 rounded border pointer-events-all whitespace-nowrap"
            style={{
              transform: `translate(-50%, -50%) translate(${labelX}px,${labelY}px)`,
              backgroundColor: hasLoop ? "#78350F" : "#1F2937",
              color: hasLoop ? "#FCD34D" : "#FDE68A",
              borderColor: hasLoop ? "#92400E" : "#4B5563",
            }}
          >
            {label}
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}
