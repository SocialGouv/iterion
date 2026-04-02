import { Handle, Position } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import type { NodeKind, AgentDecl, ToolNodeDecl, HumanDecl, JoinDecl, RouterDecl } from "@/api/types";
import { useDocumentStore } from "@/store/document";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { ProviderIcon } from "@/components/icons/ProviderIcon";

const KIND_ICONS: Record<NodeKind, string> = {
  agent: "\u{1F916}",
  judge: "\u{2696}",
  router: "\u{1F500}",
  join: "\u{1F91D}",
  human: "\u{1F464}",
  tool: "\u{1F527}",
  done: "\u{2705}",
  fail: "\u{274C}",
};

interface WorkflowNodeData extends Record<string, unknown> {
  label: string;
  kind: NodeKind;
  color: string;
  decl: unknown;
}

export default function WorkflowNode({ data }: NodeProps) {
  const { label, kind, color, decl } = data as unknown as WorkflowNodeData;
  const activeWorkflow = useActiveWorkflow();
  const diagnostics = useDocumentStore((s) => s.diagnostics);
  const isEntry = activeWorkflow?.entry === label;

  // Check if any diagnostic mentions this node (boundary match to avoid false positives)
  const hasError = diagnostics.some((d) => {
    const idx = d.indexOf(label);
    if (idx === -1) return false;
    const before = idx > 0 ? d.charAt(idx - 1) : " ";
    const after = idx + label.length < d.length ? d.charAt(idx + label.length) : " ";
    const isWordChar = (c: string) => /\w/.test(c);
    return !isWordChar(before) && !isWordChar(after);
  });

  // Extract subtitle info from declaration
  let subtitle = "";
  let providerModel: string | undefined;
  let providerDelegate: string | undefined;
  if (kind === "agent" || kind === "judge") {
    const d = decl as AgentDecl | undefined;
    providerModel = d?.model;
    providerDelegate = d?.delegate;
    if (d?.delegate) subtitle = d.delegate;
    else if (d?.model) subtitle = d.model.replace(/\$\{.*?\}/g, "env");
  } else if (kind === "tool") {
    const d = decl as ToolNodeDecl | undefined;
    if (d?.command) subtitle = d.command.length > 20 ? d.command.slice(0, 20) + "..." : d.command;
  } else if (kind === "human") {
    const d = decl as HumanDecl | undefined;
    if (d?.mode) subtitle = d.mode;
  } else if (kind === "router") {
    const d = decl as RouterDecl | undefined;
    providerModel = d?.model;
    if (d?.mode === "llm" && d?.model) subtitle = d.model.replace(/\$\{.*?\}/g, "env");
    else if (d?.mode) subtitle = d.mode;
  } else if (kind === "join") {
    const d = decl as JoinDecl | undefined;
    if (d?.strategy) subtitle = d.strategy;
  }

  // Schema badges for nodes that have input/output
  let inputSchema = "";
  let outputSchema = "";
  if (kind === "agent" || kind === "judge") {
    const d = decl as AgentDecl | undefined;
    inputSchema = d?.input ?? "";
    outputSchema = d?.output ?? "";
  } else if (kind === "human") {
    const d = decl as HumanDecl | undefined;
    inputSchema = d?.input ?? "";
    outputSchema = d?.output ?? "";
  } else if (kind === "tool") {
    const d = decl as ToolNodeDecl | undefined;
    outputSchema = d?.output ?? "";
  } else if (kind === "join") {
    const d = decl as JoinDecl | undefined;
    outputSchema = d?.output ?? "";
  }

  // Count edges
  const edgeCount = activeWorkflow?.edges?.filter(
    (e) => e.from === label || e.to === label,
  ).length ?? 0;

  // Session indicator for agents/judges
  let sessionIndicator = "";
  if (kind === "agent" || kind === "judge") {
    const d = decl as AgentDecl | undefined;
    if (d?.session === "inherit") sessionIndicator = "\u{1F517}"; // link
    else if (d?.session === "artifacts_only") sessionIndicator = "\u{1F4E6}"; // package
  }

  // Check if node participates in a loop
  const hasLoop = activeWorkflow?.edges?.some(
    (e) => e.loop && (e.from === label || e.to === label),
  ) ?? false;

  const isTerminal = kind === "done" || kind === "fail";

  return (
    <div
      className={`rounded-lg border-2 px-4 py-2 min-w-[140px] text-center shadow-lg ${isTerminal ? "opacity-80" : ""}`}
      style={{
        borderColor: hasError ? "#EF4444" : isEntry ? "#F59E0B" : color,
        background: `${color}22`,
        boxShadow: hasError
          ? "0 0 10px #EF444455"
          : isEntry
            ? "0 0 8px #F59E0B55"
            : undefined,
      }}
    >
      <Handle type="target" position={Position.Top} className="!bg-gray-400" />
      <div className="flex items-center justify-center gap-1">
        <span className="text-lg">{KIND_ICONS[kind]}</span>
        {sessionIndicator && <span className="text-xs" title={`session: ${(decl as AgentDecl)?.session}`}>{sessionIndicator}</span>}
      </div>
      <div className="font-semibold text-sm text-white">{label}</div>
      <div className="text-xs text-gray-300">{kind}</div>
      {subtitle && (
        <div className="text-[10px] text-gray-500 mt-0.5 max-w-[140px] flex items-center justify-center gap-1">
          <ProviderIcon model={providerModel} delegate={providerDelegate} size={10} className="shrink-0 opacity-70" />
          <span className="truncate">{subtitle}</span>
        </div>
      )}
      {/* Schema badges */}
      {(inputSchema || outputSchema) && (
        <div className="flex items-center justify-center gap-1 mt-1">
          {inputSchema && (
            <span className="text-[9px] bg-blue-900/50 text-blue-300 px-1 rounded" title={`input: ${inputSchema}`}>
              {"\u2192"}{inputSchema}
            </span>
          )}
          {outputSchema && (
            <span className="text-[9px] bg-green-900/50 text-green-300 px-1 rounded" title={`output: ${outputSchema}`}>
              {outputSchema}{"\u2192"}
            </span>
          )}
        </div>
      )}
      {isEntry && <div className="text-[9px] text-amber-400 mt-0.5">entry</div>}
      {hasLoop && <div className="text-[9px] text-amber-300 mt-0.5">{"\u{1F504}"} loop</div>}
      {hasError && <div className="text-[9px] text-red-400 mt-0.5">has errors</div>}
      {!isTerminal && edgeCount > 0 && (
        <div className="text-[8px] text-gray-600 mt-0.5">{edgeCount} edge{edgeCount !== 1 ? "s" : ""}</div>
      )}
      {!isTerminal && <Handle type="source" position={Position.Bottom} className="!bg-gray-400" />}
    </div>
  );
}
