import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath } from "@xyflow/react";
import type { EdgeProps } from "@xyflow/react";
import { SELECTED_BORDER, softColor } from "@/lib/constants";
import { useGroupedDiagnostics } from "@/hooks/useGroupedDiagnostics";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import DiagnosticBadge from "@/components/Diagnostics/DiagnosticBadge";

export default function ConditionalEdge(props: EdgeProps) {
  const { id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, label, data, selected, markerEnd } = props;

  const hasLoop = !!(data as Record<string, unknown>)?.loop;
  const motionReduced = useUIStore((s) => s.motionReduced);
  const grouped = useGroupedDiagnostics();
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);
  const edgeDiags = grouped.byEdge.get(id) ?? [];

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

  const strokeColor = selected
    ? SELECTED_BORDER
    : hasLoop
      ? "var(--color-warning)"
      : "var(--color-fg-subtle)";
  const strokeDasharray = hasLoop ? "8 4" : undefined;
  const strokeWidth = selected ? 3 : hasLoop ? 2.5 : 1;

  return (
    <>
      <BaseEdge
        path={edgePath}
        markerEnd={markerEnd}
        style={{
          stroke: strokeColor,
          strokeDasharray,
          strokeWidth,
          filter: selected
            ? `drop-shadow(0 0 4px ${softColor(SELECTED_BORDER, 60)})`
            : undefined,
          animation: hasLoop && !motionReduced ? "dash-flow 1s linear infinite" : undefined,
        }}
      />
      {label && (
        <EdgeLabelRenderer>
          <div
            className="absolute text-xs px-1.5 py-0.5 rounded border pointer-events-all whitespace-nowrap"
            style={{
              transform: `translate(-50%, -50%) translate(${labelX}px,${labelY}px)`,
              backgroundColor: hasLoop
                ? "var(--color-warning-soft)"
                : "var(--color-surface-1)",
              color: hasLoop
                ? "var(--color-warning-fg)"
                : "var(--color-fg-default)",
              borderColor: hasLoop
                ? "var(--color-warning)"
                : "var(--color-border-strong)",
            }}
          >
            {label}
          </div>
        </EdgeLabelRenderer>
      )}
      {edgeDiags.length > 0 && (
        <EdgeLabelRenderer>
          <div
            className="absolute pointer-events-all"
            style={{
              transform: `translate(-50%, -50%) translate(${labelX}px,${(labelY ?? 0) - (label ? 18 : 0)}px)`,
            }}
          >
            <DiagnosticBadge
              diagnostics={edgeDiags}
              onReveal={() => setSelectedEdge(id)}
            />
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}
