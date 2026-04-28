import { useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { useGroupedDiagnostics } from "@/hooks/useGroupedDiagnostics";
import { getHint } from "@/lib/diagnosticHints";
import type { AttributedDiagnostic } from "@/lib/diagnostics";
import { Cross2Icon, ChevronDownIcon, ChevronUpIcon } from "@radix-ui/react-icons";

export default function DiagnosticsPanel() {
  const document = useDocumentStore((s) => s.document);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);
  const toggleDiagnosticsPanel = useUIStore((s) => s.toggleDiagnosticsPanel);
  const grouped = useGroupedDiagnostics();
  const [showAttributed, setShowAttributed] = useState(false);

  const errorCount = grouped.all.filter((d) => d.severity === "error").length;
  const warningCount = grouped.all.length - errorCount;
  const hasIssues = grouped.all.length > 0;
  const attributedCount = grouped.all.length - grouped.global.length;

  const handleClick = (d: AttributedDiagnostic) => {
    if (!document) return;
    if (d.edgeId) setSelectedEdge(d.edgeId);
    else if (d.nodeId) setSelectedNode(d.nodeId);
  };

  return (
    <div className="px-3 py-2 text-xs h-full overflow-y-auto">
      <div className="flex items-center gap-3 mb-1.5">
        <h2 className="font-bold text-fg-muted text-sm">Diagnostics</h2>
        {hasIssues ? (
          <div className="flex items-center gap-2">
            {errorCount > 0 && (
              <span className="bg-danger-soft text-danger px-1.5 py-0.5 rounded text-[10px]">
                {errorCount} error{errorCount !== 1 ? "s" : ""}
              </span>
            )}
            {warningCount > 0 && (
              <span className="bg-warning-soft text-warning px-1.5 py-0.5 rounded text-[10px]">
                {warningCount} warning{warningCount !== 1 ? "s" : ""}
              </span>
            )}
          </div>
        ) : (
          <span className="text-success/80">No issues found.</span>
        )}
        <button
          className="ml-auto inline-flex h-5 w-5 items-center justify-center rounded text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
          onClick={toggleDiagnosticsPanel}
          aria-label="Hide diagnostics panel"
        >
          <Cross2Icon />
        </button>
      </div>

      {/* Globals (unattributed) — always shown */}
      <DiagnosticList items={grouped.global} onClick={handleClick} />

      {/* Disclosure for attributed diagnostics */}
      {attributedCount > 0 && (
        <>
          <button
            className="mt-1.5 inline-flex items-center gap-1 text-fg-subtle hover:text-fg-default"
            onClick={() => setShowAttributed((v) => !v)}
          >
            {showAttributed ? <ChevronUpIcon /> : <ChevronDownIcon />}
            {showAttributed ? "Hide" : "Show"} {attributedCount} attached to nodes/edges
          </button>
          {showAttributed && (
            <div className="mt-1">
              <DiagnosticList
                items={grouped.all.filter((d) => d.nodeId || d.edgeId)}
                onClick={handleClick}
                showAttribution
              />
            </div>
          )}
        </>
      )}
    </div>
  );
}

function DiagnosticList({
  items,
  onClick,
  showAttribution = false,
}: {
  items: AttributedDiagnostic[];
  onClick: (d: AttributedDiagnostic) => void;
  showAttribution?: boolean;
}) {
  if (items.length === 0) return null;
  return (
    <ul className="space-y-0.5">
      {items.map((d, i) => {
        const hint = d.code ? getHint(d.code) : undefined;
        const sevColor =
          d.severity === "error" ? "text-danger" : "text-warning";
        return (
          <li
            key={i}
            className={`flex items-start gap-2 cursor-pointer hover:bg-surface-2 rounded px-1 -mx-1 py-0.5 ${sevColor}`}
            onClick={() => onClick(d)}
          >
            <span className="shrink-0 font-mono text-[10px] mt-0.5 px-1 rounded bg-surface-2">
              {d.code || (d.severity === "error" ? "ERR" : "WARN")}
            </span>
            <span className="min-w-0 flex-1">
              <span className="text-fg-default">{hint?.title ?? d.message}</span>
              {hint && <span className="text-fg-subtle"> · {d.message}</span>}
              {showAttribution && (d.nodeId || d.edgeId) && (
                <span className="text-fg-subtle ml-1">
                  · {d.edgeId ? `edge ${d.edgeId}` : `node ${d.nodeId}`}
                </span>
              )}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
