import { Handle } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import type { NodeKind, AgentDecl, ToolNodeDecl, HumanDecl, RouterDecl } from "@/api/types";
import { useDocumentStore } from "@/store/document";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { ProviderIcon } from "@/components/icons/ProviderIcon";
import { NODE_ICONS } from "@/lib/constants";
import { SIDES, POS_MAP } from "./handlePositions";

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
    // Append session indicator to subtitle
    if (d?.session === "inherit") subtitle += subtitle ? " \u{1F517}" : "\u{1F517}";
    else if (d?.session === "fork") subtitle += subtitle ? " \u{1F500}" : "\u{1F500}";
    else if (d?.session === "artifacts_only") subtitle += subtitle ? " \u{1F4E6}" : "\u{1F4E6}";
  } else if (kind === "tool") {
    const d = decl as ToolNodeDecl | undefined;
    if (d?.command) subtitle = d.command.length > 20 ? d.command.slice(0, 20) + "..." : d.command;
  } else if (kind === "human") {
    const d = decl as HumanDecl | undefined;
    if (d?.mode) subtitle = d.mode;
  }

  // Append await indicator for nodes with await strategy
  if (kind === "agent" || kind === "judge" || kind === "human" || kind === "tool") {
    const awaitVal = (decl as AgentDecl | HumanDecl | ToolNodeDecl | undefined)?.await;
    if (awaitVal && awaitVal !== "none") {
      subtitle += subtitle ? ` \u{23F3}` : "\u{23F3}";
    }
  }

  if (kind === "router") {
    const d = decl as RouterDecl | undefined;
    providerModel = d?.model;
    if (d?.mode === "llm" && d?.model) subtitle = d.model.replace(/\$\{.*?\}/g, "env");
    else if (d?.mode) subtitle = d.mode;
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
  }

  // Check if node participates in a loop
  const hasLoop = activeWorkflow?.edges?.some(
    (e) => e.loop && (e.from === label || e.to === label),
  ) ?? false;

  const isTerminal = kind === "done" || kind === "fail";
  const isStart = kind === "start";

  return (
    <div
      className={`rounded-lg border-2 px-4 py-2 min-w-[140px] text-center shadow-lg ${isTerminal || isStart ? "opacity-80" : ""}`}
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
      {!isStart && SIDES.map(s => (
        <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-gray-400 !w-1.5 !h-1.5 !opacity-0" />
      ))}
      <div className="flex items-center justify-center gap-1">
        <span className="text-lg">{NODE_ICONS[kind]}</span>
      </div>
      <div className="font-semibold text-sm text-white">{isStart ? "Start" : label}</div>
      {!isStart && <div className="text-xs text-gray-300">{kind}</div>}
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
      {/* Compact status badges: entry, loop, error — single row */}
      {(isEntry || hasLoop || hasError) && (
        <div className="flex items-center justify-center gap-1.5 mt-1">
          {isEntry && <span className="text-[9px] bg-amber-900/50 text-amber-400 px-1 rounded">entry</span>}
          {hasLoop && <span className="text-[9px] bg-amber-800/50 text-amber-300 px-1 rounded">{"\u{1F504}"} loop</span>}
          {hasError && <span className="text-[9px] bg-red-900/50 text-red-400 px-1 rounded">error</span>}
        </div>
      )}
      {!isTerminal && SIDES.map(s => (
        <Handle key={`source-${s}`} id={`source-${s}`} type="source" position={POS_MAP[s]} className="!bg-gray-400 !w-1.5 !h-1.5 !opacity-0" />
      ))}
    </div>
  );
}
