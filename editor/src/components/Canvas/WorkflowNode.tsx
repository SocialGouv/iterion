import { Handle, Position } from "@xyflow/react";
import type { NodeProps } from "@xyflow/react";
import type { NodeKind, AgentDecl, ToolNodeDecl } from "@/api/types";
import { useDocumentStore } from "@/store/document";

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
  const document = useDocumentStore((s) => s.document);
  const isEntry = document?.workflows?.[0]?.entry === label;

  // Extract subtitle info from declaration
  let subtitle = "";
  if (kind === "agent" || kind === "judge") {
    const d = decl as AgentDecl | undefined;
    if (d?.delegate) subtitle = d.delegate;
    else if (d?.model) subtitle = d.model.replace(/\$\{.*?\}/g, "env");
  } else if (kind === "tool") {
    const d = decl as ToolNodeDecl | undefined;
    if (d?.command) subtitle = d.command.length > 20 ? d.command.slice(0, 20) + "..." : d.command;
  }

  return (
    <div
      className="rounded-lg border-2 px-4 py-2 min-w-[120px] text-center shadow-lg"
      style={{
        borderColor: isEntry ? "#F59E0B" : color,
        background: `${color}22`,
        boxShadow: isEntry ? "0 0 8px #F59E0B55" : undefined,
      }}
    >
      <Handle type="target" position={Position.Top} className="!bg-gray-400" />
      <div className="text-lg">{KIND_ICONS[kind]}</div>
      <div className="font-semibold text-sm text-white">{label}</div>
      <div className="text-xs text-gray-300">{kind}</div>
      {subtitle && <div className="text-[10px] text-gray-500 mt-0.5 truncate max-w-[140px]">{subtitle}</div>}
      {isEntry && <div className="text-[9px] text-amber-400 mt-0.5">entry</div>}
      <Handle type="source" position={Position.Bottom} className="!bg-gray-400" />
    </div>
  );
}
