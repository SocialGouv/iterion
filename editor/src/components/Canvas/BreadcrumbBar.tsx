import { useUIStore } from "@/store/ui";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";

export default function BreadcrumbBar() {
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const popSubNodeView = useUIStore((s) => s.popSubNodeView);
  const clearSubNodeView = useUIStore((s) => s.clearSubNodeView);
  const navigateSubNodeViewTo = useUIStore((s) => s.navigateSubNodeViewTo);
  const activeWorkflow = useActiveWorkflow();

  if (subNodeViewStack.length === 0) return null;

  const workflowName = activeWorkflow?.name ?? "workflow";

  return (
    <div className="absolute top-2 left-1/2 -translate-x-1/2 z-20 flex items-center gap-1 bg-surface-0/90 border border-border-default rounded-lg px-3 py-1.5 shadow-lg backdrop-blur-sm">
      {/* Back button */}
      <button
        className="text-fg-subtle hover:text-fg-default text-sm mr-1 px-1"
        onClick={popSubNodeView}
        title="Go back (Esc)"
      >
        {"\u2190"}
      </button>

      {/* Workflow root */}
      <button
        className="text-xs text-accent hover:text-accent transition-colors"
        onClick={clearSubNodeView}
      >
        {workflowName}
      </button>

      {/* Stack segments */}
      {subNodeViewStack.map((nodeId, index) => (
        <span key={index} className="flex items-center gap-1">
          <span className="text-fg-subtle text-xs">{"\u203A"}</span>
          {index < subNodeViewStack.length - 1 ? (
            <button
              className="text-xs text-accent hover:text-accent transition-colors"
              onClick={() => navigateSubNodeViewTo(index)}
            >
              {nodeId}
            </button>
          ) : (
            <span className="text-xs text-fg-default font-medium">{nodeId}</span>
          )}
        </span>
      ))}
    </div>
  );
}
