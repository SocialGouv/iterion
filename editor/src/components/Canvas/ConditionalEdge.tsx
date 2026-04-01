import { BaseEdge, EdgeLabelRenderer, getBezierPath } from "@xyflow/react";
import type { EdgeProps } from "@xyflow/react";

export default function ConditionalEdge(props: EdgeProps) {
  const { sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, label, data } = props;
  const [edgePath, labelX, labelY] = getBezierPath({
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
  });

  const hasLoop = !!(data as Record<string, unknown>)?.loop;
  const strokeColor = hasLoop ? "#F59E0B" : "#888";
  const strokeDasharray = hasLoop ? "6 3" : undefined;

  return (
    <>
      <BaseEdge path={edgePath} style={{ stroke: strokeColor, strokeDasharray, strokeWidth: hasLoop ? 2 : 1 }} />
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
