import { Handle } from "@xyflow/react";
import type { Node, NodeProps } from "@xyflow/react";
import type { NodeKind } from "@/api/types";
import { declOf } from "@/lib/documentToGraph";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { useGroupedDiagnostics } from "@/hooks/useGroupedDiagnostics";
import { useSelectionStore } from "@/store/selection";
import { dominantSeverity } from "@/lib/diagnostics";
import { ProviderIcon } from "@/components/icons/ProviderIcon";
import { BackendBadge } from "@/components/icons/BackendBadge";
import DiagnosticBadge from "@/components/Diagnostics/DiagnosticBadge";
import { EffortBar, isEffortLevel } from "@/components/ui/EffortBar";
import { effortBackendKey, useEffortCapabilities } from "@/hooks/useEffortCapabilities";
import { useResolvedEffort } from "@/hooks/useResolvedEffort";
import { SELECTED_BORDER, SELECTED_GLOW, softColor } from "@/lib/constants";
import { NodeIcon } from "@/components/icons/NodeIcon";
import { SIDES, POS_MAP } from "./handlePositions";

interface WorkflowNodeData extends Record<string, unknown> {
  label: string;
  kind: NodeKind;
  color: string;
  decl: unknown;
  // Set on nodes belonging to an expanded subbot's child graph — rendered
  // dimmed and read-only (the diagnostics/entry/loop badges belong to the
  // PARENT document and never apply to them).
  external?: boolean;
  subbotSource?: string;
  // Set on a compact subbot node whose child file failed to load.
  loadError?: string;
}

type WorkflowNodeType = Node<WorkflowNodeData, "workflow">;

export default function WorkflowNode({ data, selected }: NodeProps<WorkflowNodeType>) {
  const { label, kind, color, decl, external, subbotSource, loadError } = data;
  const activeWorkflow = useActiveWorkflow();
  const grouped = useGroupedDiagnostics();
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const isEntry = !external && activeWorkflow?.entry === label;

  const nodeDiags = external ? [] : grouped.byNode.get(label) ?? [];
  const severity = dominantSeverity(nodeDiags);
  const hasError = severity === "error";
  const hasWarning = severity === "warning";

  // Two-row meta layout for LLM-driving nodes (agent/judge/router-llm):
  //   row 1: provider icon + model
  //   row 2: backend + reasoning_effort + session/await indicators
  // Other node kinds keep the legacy single-line subtitle.
  let subtitle = "";
  let providerModel: string | undefined;
  let providerDelegate: string | undefined;
  let backendValue: string | undefined;
  let effortLevel: string | undefined;
  let sessionGlyph: string | undefined;
  let awaitGlyph: string | undefined;
  let isLLMNode = false;

  if (kind === "agent" || kind === "judge") {
    const d = declOf(kind, decl);
    isLLMNode = !!(d?.model || d?.backend);
    providerModel = d?.model;
    providerDelegate = d?.backend;
    backendValue = d?.backend;
    effortLevel = d?.reasoning_effort;
    if (d?.session === "inherit") sessionGlyph = "\u{1F517}";
    else if (d?.session === "fork") sessionGlyph = "\u{1F500}";
    else if (d?.session === "artifacts_only") sessionGlyph = "\u{1F4E6}";
    if (d?.await && d.await !== "none") awaitGlyph = "\u{23F3}";
  } else if (kind === "router") {
    const d = declOf(kind, decl);
    if (d?.mode === "llm") {
      isLLMNode = true;
      providerModel = d?.model;
      providerDelegate = d?.backend;
      backendValue = d?.backend;
      // RouterDecl does not surface reasoning_effort in the studio wire
      // format yet; the IR carries it but the form/JSON does not.
    } else if (d?.mode) {
      subtitle = d.mode;
    }
  } else if (kind === "tool") {
    const d = declOf(kind, decl);
    if (d?.command) subtitle = d.command.length > 20 ? d.command.slice(0, 20) + "..." : d.command;
    if (d?.await && d.await !== "none") awaitGlyph = "\u{23F3}";
  } else if (kind === "human") {
    const d = declOf(kind, decl);
    if (d?.interaction) subtitle = d.interaction;
    if (d?.await && d.await !== "none") awaitGlyph = "\u{23F3}";
  } else if (kind === "compute") {
    const d = declOf(kind, decl);
    if (d?.expr?.length) subtitle = `${d.expr.length} expr`;
  } else if (kind === "subbot") {
    // Compact form only: shown while the child document is loading, failed
    // to load, or the subbot sits in a collapsed group. Expanded subbots
    // render as a subbotFrame container instead of this node.
    const d = declOf(kind, decl);
    if (d?.source) subtitle = d.source;
  }

  const modelLabel = providerModel
    ? providerModel.replace(/\$\{.*?\}/g, "env")
    : undefined;

  // Substitute env-subst literals so the bar shows the actual level
  // ("max") instead of the raw "${VAR:-max}". Capabilities feed the
  // bar's normalisation: a gpt-5 node at "high" renders 4/4 cells.
  const resolvedEffort = useResolvedEffort(effortLevel);
  const { capabilities: effortCaps } = useEffortCapabilities(
    isLLMNode && providerModel ? effortBackendKey(backendValue) : undefined,
    isLLMNode ? providerModel : undefined,
  );
  const effortSupported = effortCaps?.supported ?? undefined;

  // Schema badges for nodes that have input/output
  let inputSchema = "";
  let outputSchema = "";
  if (kind === "agent" || kind === "judge") {
    const d = declOf(kind, decl);
    inputSchema = d?.input ?? "";
    outputSchema = d?.output ?? "";
  } else if (kind === "human") {
    const d = declOf(kind, decl);
    inputSchema = d?.input ?? "";
    outputSchema = d?.output ?? "";
  } else if (kind === "tool") {
    const d = declOf(kind, decl);
    outputSchema = d?.output ?? "";
  }

  // Check if node participates in a loop
  const hasLoop = !external && (activeWorkflow?.edges?.some(
    (e) => e.loop && (e.from === label || e.to === label),
  ) ?? false);

  const isTerminal = kind === "done" || kind === "fail";
  const isStart = kind === "start";

  const borderColor = selected
    ? SELECTED_BORDER
    : hasError
      ? "var(--color-danger)"
      : hasWarning || isEntry
        ? "var(--color-warning)"
        : color;
  const glow = selected
    ? SELECTED_GLOW
    : hasError
      ? `0 0 10px ${softColor("var(--color-danger)", 33)}`
      : hasWarning
        ? `0 0 8px ${softColor("var(--color-warning)", 27)}`
        : isEntry
          ? `0 0 8px ${softColor("var(--color-warning)", 33)}`
          : undefined;

  return (
    <div
      className={`relative rounded-xl border-2 px-4 py-2 min-w-[140px] text-center shadow-[var(--shadow-sm)] hover:shadow-[var(--shadow-md)] transition-shadow ${isTerminal || isStart ? "opacity-80" : ""}`}
      style={{
        borderColor,
        background: softColor(color, 10),
        boxShadow: glow,
        // Display-only child of an expanded subbot: dimmed + desaturated so
        // it reads as "not part of the edited bot".
        ...(external && { opacity: 0.75, filter: "saturate(0.7)" }),
      }}
      title={external ? `Part of ${subbotSource || "a subbot"} — open the file to edit` : undefined}
    >
      {nodeDiags.length > 0 && (
        <div className="absolute -top-2 -right-2 z-[var(--z-canvas)]">
          <DiagnosticBadge
            diagnostics={nodeDiags}
            onReveal={() => setSelectedNode(label)}
          />
        </div>
      )}
      {loadError && (
        // -left-2 (not -right-2): the top-right slot belongs to
        // DiagnosticBadge, and a compact subbot can carry both.
        <div
          role="img"
          aria-label={`Failed to load subbot source: ${loadError}`}
          className="absolute -top-2 -left-2 z-[var(--z-canvas)] w-4 h-4 rounded-full bg-warning-soft text-warning-fg text-caption leading-4 border border-warning"
          title={`Failed to load subbot source: ${loadError}`}
        >
          !
        </div>
      )}
      {!isStart && SIDES.map(s => (
        <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
      ))}
      <div className="flex items-center justify-center gap-1">
        <NodeIcon kind={kind} size={20} />
      </div>
      <div className="font-semibold text-sm text-fg-default">{isStart ? "Start" : label}</div>
      {!isStart && <div className="text-xs text-fg-muted">{kind}</div>}
      {isLLMNode && modelLabel && (
        <div className="text-caption text-fg-subtle mt-0.5 max-w-[160px] flex items-center justify-center gap-1">
          <ProviderIcon model={providerModel} delegate={providerDelegate} size={10} className="shrink-0 opacity-70" />
          <span className="truncate">{modelLabel}</span>
        </div>
      )}
      {isLLMNode && (
        <div className="text-caption text-fg-subtle mt-0.5 max-w-[160px] flex items-center justify-center gap-1.5 flex-wrap">
          <BackendBadge backend={backendValue} size={10} />
          {isEffortLevel(resolvedEffort) ? (
            <EffortBar
              level={resolvedEffort}
              supported={effortSupported}
              title={
                effortLevel && effortLevel !== resolvedEffort
                  ? `reasoning_effort: ${resolvedEffort} (resolved from ${effortLevel})`
                  : undefined
              }
            />
          ) : effortLevel && effortLevel.includes("$") ? (
            // Resolution failed (invalid env value, server unreachable);
            // fall back to the literal so the author still sees what
            // they wrote.
            <span
              className="font-mono text-[9px] text-fg-muted truncate max-w-[100px]"
              title={`reasoning_effort: ${effortLevel} (env-substituted at runtime)`}
            >
              {effortLevel}
            </span>
          ) : null}
          {sessionGlyph && <span aria-hidden>{sessionGlyph}</span>}
          {awaitGlyph && <span aria-hidden>{awaitGlyph}</span>}
        </div>
      )}
      {!isLLMNode && subtitle && (
        <div className="text-caption text-fg-subtle mt-0.5 max-w-[160px] flex items-center justify-center gap-1">
          <span className="truncate">{subtitle}</span>
          {awaitGlyph && <span aria-hidden>{awaitGlyph}</span>}
        </div>
      )}
      {/* Schema badges */}
      {(inputSchema || outputSchema) && (
        <div className="flex items-center justify-center gap-1 mt-1">
          {inputSchema && (
            <span className="text-[9px] bg-accent-soft text-accent-text px-1 rounded" title={`input: ${inputSchema}`}>
              {"\u2192"}{inputSchema}
            </span>
          )}
          {outputSchema && (
            <span className="text-[9px] bg-success-soft text-success-fg px-1 rounded" title={`output: ${outputSchema}`}>
              {outputSchema}{"\u2192"}
            </span>
          )}
        </div>
      )}
      {/* Compact status badges: entry, loop. Diagnostic badge handled separately. */}
      {(isEntry || hasLoop) && (
        <div className="flex items-center justify-center gap-1.5 mt-1">
          {isEntry && <span className="text-[9px] bg-warning-soft text-warning-fg px-1 rounded">entry</span>}
          {hasLoop && <span className="text-[9px] bg-warning-soft text-warning-fg px-1 rounded">{"\u{1F504}"} loop</span>}
        </div>
      )}
      {/* An expanded subbot's `done` node carries the parent's outgoing
          edge (frame -> downstream), so external terminals DO need source
          handles; user connections from them are rejected upstream. */}
      {(!isTerminal || external) && SIDES.map(s => (
        <Handle key={`source-${s}`} id={`source-${s}`} type="source" position={POS_MAP[s]} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
      ))}
    </div>
  );
}
