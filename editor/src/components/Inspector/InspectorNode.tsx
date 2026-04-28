import { useMemo, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { NODE_COLORS, NODE_ICONS } from "@/lib/constants";
import type {
  AgentDecl,
  HumanDecl,
  JudgeDecl,
  NodeKind,
  RouterDecl,
  ToolNodeDecl,
} from "@/api/types";
import AgentForm from "@/components/Panels/forms/AgentForm";
import RouterForm from "@/components/Panels/forms/RouterForm";
import HumanForm from "@/components/Panels/forms/HumanForm";
import ToolForm from "@/components/Panels/forms/ToolForm";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { IconButton } from "@/components/ui";
import { TrashIcon } from "@radix-ui/react-icons";

const TERMINAL_DESCRIPTIONS: Record<string, string> = {
  __start__: "Marks the workflow entry point.",
  done: "Terminal node — workflow success.",
  fail: "Terminal node — workflow failure.",
};

const TERMINAL_LABELS: Record<string, string> = {
  __start__: "Start",
  done: "Done",
  fail: "Fail",
};

interface NodeMatch {
  kind: NodeKind;
  decl: AgentDecl | JudgeDecl | RouterDecl | HumanDecl | ToolNodeDecl;
}

export default function InspectorNode({ nodeId }: { nodeId: string }) {
  const document = useDocumentStore((s) => s.document);
  const removeNode = useDocumentStore((s) => s.removeNode);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const match = useMemo<NodeMatch | null>(() => {
    if (!document) return null;
    for (const a of document.agents) if (a.name === nodeId) return { kind: "agent", decl: a };
    for (const j of document.judges) if (j.name === nodeId) return { kind: "judge", decl: j };
    for (const r of document.routers) if (r.name === nodeId) return { kind: "router", decl: r };
    for (const h of document.humans) if (h.name === nodeId) return { kind: "human", decl: h };
    for (const t of document.tools) if (t.name === nodeId) return { kind: "tool", decl: t };
    return null;
  }, [document, nodeId]);

  // Terminal nodes
  if (TERMINAL_DESCRIPTIONS[nodeId]) {
    const icon = NODE_ICONS[nodeId === "__start__" ? "start" : (nodeId as NodeKind)] ?? "";
    return (
      <div className="p-3">
        <div className="flex items-center gap-3 rounded-md border border-border-default bg-surface-1 px-3 py-3">
          <span className="text-xl">{icon}</span>
          <div>
            <p className="text-sm font-semibold text-fg-default">
              {TERMINAL_LABELS[nodeId]}
            </p>
            <p className="text-xs text-fg-subtle mt-0.5">{TERMINAL_DESCRIPTIONS[nodeId]}</p>
          </div>
        </div>
      </div>
    );
  }

  if (!match) {
    return (
      <div className="p-3 text-xs text-fg-subtle">
        Node "{nodeId}" not found in the current document.
      </div>
    );
  }

  const handleDelete = () => {
    removeNode(nodeId);
    clearSelection();
    setConfirmDelete(false);
  };

  const handleRename = (newName: string) => {
    if (!newName.trim() || newName === nodeId) return;
    renameNode(nodeId, newName);
    setSelectedNode(newName);
  };

  return (
    <div className="h-full flex flex-col">
      <NodeHeader
        kind={match.kind}
        name={nodeId}
        onRename={handleRename}
        onDelete={() => setConfirmDelete(true)}
      />
      <div className="flex-1 overflow-y-auto p-3">
        <NodeForm match={match} />
      </div>
      <ConfirmDialog
        open={confirmDelete}
        title="Delete Node"
        message={`Delete "${nodeId}"? This will also remove all edges connected to it.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={handleDelete}
        onCancel={() => setConfirmDelete(false)}
      />
    </div>
  );
}

function NodeHeader({
  kind,
  name,
  onRename,
  onDelete,
}: {
  kind: NodeKind;
  name: string;
  onRename: (newName: string) => void;
  onDelete: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(name);
  const color = NODE_COLORS[kind];
  const icon = NODE_ICONS[kind];

  const commit = () => {
    if (draft.trim() && draft !== name) onRename(draft.trim());
    setEditing(false);
  };

  return (
    <div className="flex items-center gap-2 border-b border-border-default px-3 py-2 shrink-0">
      <span
        className="inline-flex h-6 w-6 items-center justify-center rounded-md text-sm shrink-0"
        style={{ background: `${color}33`, color }}
        title={kind}
      >
        {icon}
      </span>
      <div className="min-w-0 flex-1">
        {editing ? (
          <input
            autoFocus
            className="w-full bg-surface-2 border border-accent rounded px-1.5 py-0.5 text-sm text-fg-default outline-none"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={commit}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                commit();
              } else if (e.key === "Escape") {
                e.preventDefault();
                setDraft(name);
                setEditing(false);
              }
            }}
          />
        ) : (
          <button
            type="button"
            className="text-sm font-semibold text-fg-default truncate hover:text-accent w-full text-left"
            onClick={() => {
              setDraft(name);
              setEditing(true);
            }}
            title="Click to rename"
          >
            {name}
          </button>
        )}
        <div className="text-[10px] uppercase tracking-wider text-fg-subtle">{kind}</div>
      </div>
      <IconButton
        variant="ghost"
        size="sm"
        label="Delete node"
        onClick={onDelete}
      >
        <TrashIcon />
      </IconButton>
    </div>
  );
}

function NodeForm({ match }: { match: NodeMatch }) {
  switch (match.kind) {
    case "agent":
      return <AgentForm decl={match.decl as AgentDecl} kind="agent" />;
    case "judge":
      return <AgentForm decl={match.decl as JudgeDecl} kind="judge" />;
    case "router":
      return <RouterForm decl={match.decl as RouterDecl} />;
    case "human":
      return <HumanForm decl={match.decl as HumanDecl} />;
    case "tool":
      return <ToolForm decl={match.decl as ToolNodeDecl} />;
    default:
      return <p className="text-fg-subtle text-xs">Terminal node (no editable properties)</p>;
  }
}

