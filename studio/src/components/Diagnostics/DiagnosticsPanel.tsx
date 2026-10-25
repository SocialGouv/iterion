import { useState } from "react";
import { clickableRowProps } from "@/lib/a11y";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { useGroupedDiagnostics } from "@/hooks/useGroupedDiagnostics";
import { getHint } from "@/lib/diagnosticHints";
import { countOf, type AttributedDiagnostic } from "@/lib/diagnostics";
import { Button } from "@/components/ui/Button";
import { IconButton } from "@/components/ui/IconButton";
import { Input } from "@/components/ui/Input";
import { Cross2Icon, ChevronDownIcon, ChevronUpIcon } from "@radix-ui/react-icons";

export default function DiagnosticsPanel() {
  const document = useDocumentStore((s) => s.document);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);
  const toggleDiagnosticsPanel = useUIStore((s) => s.toggleDiagnosticsPanel);
  const grouped = useGroupedDiagnostics();
  const [showAttributed, setShowAttributed] = useState(false);
  const [search, setSearch] = useState("");
  const [showErrorsOnly, setShowErrorsOnly] = useState(false);

  // Apply the search query + errors-only filter to every diagnostic
  // before we slice into globals / attributed. Keeps the two lists in
  // sync — a search like "C034" hides global *and* attributed
  // diagnostics that don't match.
  const visible = grouped.all.filter((d) => {
    if (showErrorsOnly && d.severity !== "error") return false;
    const q = search.trim().toLowerCase();
    if (!q) return true;
    const hay = `${d.code ?? ""}\t${d.message}\t${d.nodeId ?? ""}\t${d.edgeId ?? ""}`.toLowerCase();
    return hay.includes(q);
  });
  const visibleGlobal = visible.filter((d) => !(d.nodeId || d.edgeId));
  const visibleAttributed = visible.filter((d) => d.nodeId || d.edgeId);

  // The readings' counts, not the list's length: a reading reports at most
  // 1000 of each code, and says how many more it counted (unreportedBehind).
  const errorCount = countOf(grouped.all, "error");
  const warningCount = countOf(grouped.all, "warning");
  const hasIssues = grouped.all.length > 0;
  const filterActive = search.trim() !== "" || showErrorsOnly;
  const attributedCount = visibleAttributed.length;

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
              <span className="bg-danger-soft text-danger-fg px-1.5 py-0.5 rounded text-caption">
                {errorCount} error{errorCount !== 1 ? "s" : ""}
              </span>
            )}
            {warningCount > 0 && (
              <span className="bg-warning-soft text-warning-fg px-1.5 py-0.5 rounded text-caption">
                {warningCount} warning{warningCount !== 1 ? "s" : ""}
              </span>
            )}
          </div>
        ) : (
          <span className="text-success-fg">No issues found.</span>
        )}
        <IconButton
          label="Hide diagnostics"
          size="sm"
          variant="ghost"
          className="ml-auto"
          onClick={toggleDiagnosticsPanel}
        >
          <Cross2Icon />
        </IconButton>
      </div>

      {hasIssues && (
        <div className="flex items-center gap-2 mb-2">
          <Input
            size="sm"
            type="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search code or message…"
            aria-label="Filter diagnostics"
            className="flex-1"
          />
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setShowErrorsOnly((v) => !v)}
            aria-pressed={showErrorsOnly}
            className={
              showErrorsOnly
                ? "bg-danger-soft border border-danger text-danger-fg"
                : "border border-border-default text-fg-subtle"
            }
            title="Show only error-severity diagnostics"
          >
            errors only
          </Button>
          {filterActive && (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                setSearch("");
                setShowErrorsOnly(false);
              }}
              className="underline"
            >
              reset
            </Button>
          )}
        </div>
      )}

      {/* Globals (unattributed) — always shown */}
      <DiagnosticList items={visibleGlobal} onClick={handleClick} />

      {filterActive && visible.length === 0 && (
        <div className="text-fg-subtle text-micro py-2">No diagnostics match.</div>
      )}

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
                items={visibleAttributed}
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
        // Composite key: code+attribution+message is stable across
        // reorders (array index is not — animations / focus / aria
        // tooltips get reused on the wrong row otherwise).
        const stableKey = `${d.code ?? ""}|${d.nodeId ?? d.edgeId ?? ""}|${d.message}`;
        const codeLabel = d.code || (d.severity === "error" ? "ERR" : "WARN");
        const titleText = hint?.title ?? d.message;
        const attributionText = showAttribution && (d.nodeId || d.edgeId)
          ? d.edgeId
            ? `edge ${d.edgeId}`
            : `node ${d.nodeId}`
          : null;
        return (
          <li
            key={stableKey || i}
            // No aria-label: the visible row IS the accessible name
            // (RGAA 7.1 — a name must contain what is rendered).
            {...clickableRowProps(() => onClick(d))}
            className={`flex items-start gap-2 cursor-pointer hover:bg-surface-2 focus-visible:bg-surface-2 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent rounded px-1 -mx-1 py-0.5 ${sevColor}`}
          >
            <span className="shrink-0 font-mono text-caption mt-0.5 px-1 rounded bg-surface-2">
              {codeLabel}
            </span>
            <span className="min-w-0 flex-1">
              <span className="text-fg-default">{titleText}</span>
              {hint && <span className="text-fg-subtle"> · {d.message}</span>}
              {d.hint && <span className="block text-fg-subtle italic">{d.hint}</span>}
              {attributionText && (
                <span className="text-fg-subtle ml-1">
                  · {attributionText}
                </span>
              )}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
