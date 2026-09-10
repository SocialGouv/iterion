import { useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useTabsStore } from "@/store/tabs";
import { NODE_COLORS, NODE_ICONS, softColor } from "@/lib/constants";
import { getAllNodeNames } from "@/lib/defaults";
import {
  parseSubbotChildId,
  resolveChildBotOpenPath,
} from "@/lib/subbotGraph";
import type {
  AgentDecl,
  ComputeDecl,
  FailDecl,
  HumanDecl,
  JudgeDecl,
  NodeKind,
  RouterDecl,
  SubbotDecl,
  ToolNodeDecl,
} from "@/api/types";
import AgentForm from "@/components/Panels/forms/AgentForm";
import RouterForm from "@/components/Panels/forms/RouterForm";
import HumanForm from "@/components/Panels/forms/HumanForm";
import ToolForm from "@/components/Panels/forms/ToolForm";
import ComputeForm from "@/components/Panels/forms/ComputeForm";
import { CheckboxField, CommittedTextField, NodeFormHeader, SelectFieldWithCreate, TextField } from "@/components/Panels/forms/FormField";
import { useSchemaPromptCreators } from "@/hooks/useSchemaPromptCreators";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import NodeRunsChip from "./NodeRunsChip";
import { Button, IconButton } from "@/components/ui";
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

// Discriminated on `kind` so NodeForm's switch narrows `decl` to the
// matching declaration type without casts.
type NodeMatch =
  | { kind: "agent"; decl: AgentDecl }
  | { kind: "judge"; decl: JudgeDecl }
  | { kind: "router"; decl: RouterDecl }
  | { kind: "human"; decl: HumanDecl }
  | { kind: "tool"; decl: ToolNodeDecl }
  | { kind: "compute"; decl: ComputeDecl }
  | { kind: "subbot"; decl: SubbotDecl }
  | { kind: "fail"; decl: FailDecl };

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
    for (const c of document.computes ?? []) if (c.name === nodeId) return { kind: "compute", decl: c };
    for (const sb of document.subbots ?? []) if (sb.name === nodeId) return { kind: "subbot", decl: sb };
    // A named `fail <name>:` is drawn on the canvas, so it must resolve
    // here too — otherwise selecting the node the canvas just rendered
    // reports it "not found in the current document".
    for (const f of document.fails ?? []) if (f.name === nodeId) return { kind: "fail", decl: f };
    return null;
  }, [document, nodeId]);

  // Node inside an expanded subbot frame — belongs to the CHILD file, so
  // there is nothing to edit here; show a read-only notice + open button.
  const external = parseSubbotChildId(nodeId);
  if (external) {
    return <SubbotChildNotice subbotId={external.subbotId} childId={external.childId} />;
  }

  // Terminal nodes
  if (TERMINAL_DESCRIPTIONS[nodeId]) {
    // The guard above restricts nodeId to __start__/done/fail.
    const iconKind: NodeKind =
      nodeId === "__start__" ? "start" : nodeId === "fail" ? "fail" : "done";
    const icon = NODE_ICONS[iconKind] ?? "";
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
      <NodeRunsChip nodeId={nodeId} />
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
  // Re-sync `draft` whenever the canonical `name` prop changes from
  // the outside (e.g. an AgentForm CommittedTextField rename routed
  // through the store). Without this, clicking the header to edit
  // shows the pre-rename draft and committing it would silently
  // rename back to the old name.
  useEffect(() => {
    if (!editing) setDraft(name);
  }, [name, editing]);
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
        style={{ background: softColor(color, 20), color }}
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
            className="text-sm font-semibold text-fg-default truncate hover:text-accent-text w-full text-left"
            onClick={() => {
              setDraft(name);
              setEditing(true);
            }}
            title="Click to rename"
          >
            {name}
          </button>
        )}
        <div className="text-caption uppercase tracking-wider text-fg-subtle">{kind}</div>
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
      return <AgentForm decl={match.decl} kind="agent" />;
    case "judge":
      return <AgentForm decl={match.decl} kind="judge" />;
    case "router":
      return <RouterForm decl={match.decl} />;
    case "human":
      return <HumanForm decl={match.decl} />;
    case "tool":
      return <ToolForm decl={match.decl} />;
    case "compute":
      return <ComputeForm decl={match.decl} />;
    case "subbot":
      return <SubbotForm decl={match.decl} />;
    case "fail":
      return <FailPanel decl={match.decl} />;
  }
}

/** Read-only view of a declared terminal failure. The studio editor has no
 *  form for it yet: `code:` is compile-validated (C247/C248) and `message:`
 *  is a template whose refs are checked against the graph, so an
 *  unvalidated free-text field here would let the editor produce a .bot the
 *  compiler then rejects. Showing what the node declares is what the
 *  inspector owes the canvas that already draws it. */
function FailPanel({ decl }: { decl: FailDecl }) {
  const rows: Array<[string, string]> = [
    ["Code", decl.code ?? "— (untyped: the run reports FAIL_NODE)"],
    ["Message", decl.message ?? "— (the run reports the generic wording)"],
    ["Resumable", decl.resumable ? "yes — parks failed_resumable on the guard that routed in" : "no — terminal failed"],
  ];
  return (
    <div className="space-y-1">
      <NodeFormHeader color={NODE_COLORS.fail} icon={NODE_ICONS.fail} label="Fail" />
      {decl.description ? (
        <p className="px-1 pb-2 text-xs text-fg-subtle">{decl.description}</p>
      ) : null}
      <dl className="space-y-2 px-1">
        {rows.map(([label, value]) => (
          <div key={label}>
            <dt className="text-caption font-medium uppercase tracking-wide text-fg-subtle">{label}</dt>
            <dd className="mt-0.5 break-words text-xs text-fg-default">{value}</dd>
          </div>
        ))}
      </dl>
      <p className="px-1 pt-2 text-caption text-fg-subtle">
        Edit these in the .bot source — the code and the message template are
        validated at compile time.
      </p>
    </div>
  );
}

/** Opens a subbot's child .bot file in its own editor tab (same flow as
 *  RecentFilesPanel: openTab + URL so deep-link state stays in sync). */
function useOpenChildBot() {
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const activeEditorFile = useTabsStore((s) => {
    const active = s.tabs.find((tab) => tab.id === s.activeEditorTabId);
    return active?.kind === "editor" ? (active.params.file ?? null) : null;
  });
  const [, setLocation] = useLocation();
  return (source: string | undefined) => {
    if (!source) return;
    // currentFilePath is normally authoritative. The tab binding is a safe
    // fallback during route hydration: Pipelines may activate the editor tab
    // one render before EditorTabHost has copied its file into the document
    // store. Never send a parent-relative source to the workspace-root API.
    const path = resolveChildBotOpenPath(
      currentFilePath,
      activeEditorFile,
      source,
    );
    if (!path) return;
    useTabsStore.getState().openTab("editor", { file: path });
    setLocation(`/editor?file=${encodeURIComponent(path)}`);
  };
}

function SubbotForm({ decl }: { decl: SubbotDecl }) {
  const document = useDocumentStore((s) => s.document);
  const updateSubbot = useDocumentStore((s) => s.updateSubbot);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const { createSchema } = useSchemaPromptCreators();
  const openChild = useOpenChildBot();

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));
  const withEntries = decl.with ?? [];

  return (
    <div className="space-y-1">
      <NodeFormHeader color={NODE_COLORS.subbot} icon={NODE_ICONS.subbot} label="Subbot" />
      <CommittedTextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
        onCommit={(v) => setSelectedNode(v)}
        validate={(v) => {
          if (!v.trim()) return "Name cannot be empty";
          if (/\s/.test(v)) return "Name cannot contain spaces";
          const names = getAllNodeNames(document!);
          names.delete(decl.name);
          if (names.has(v)) return "Name already exists";
          return null;
        }}
      />
      <TextField
        label="Source"
        value={decl.source ?? ""}
        onChange={(v) => updateSubbot(decl.name, { source: v || undefined })}
        placeholder="child.bot"
        help="Path of the child .bot file, relative to this file. The child runs as a separate run per invocation."
      />
      <SelectFieldWithCreate
        label="Output Schema"
        value={decl.output ?? ""}
        onChange={(v) => updateSubbot(decl.name, { output: v || undefined })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- none --"
        onCreate={createSchema}
        help="Optional. Schema the child run's final output is validated against."
      />
      <CheckboxField
        label="Isolated"
        checked={decl.isolated ?? false}
        onChange={(v) => updateSubbot(decl.name, { isolated: v || undefined })}
        help="Assert the child confines its writes to its own run store — required for parallel fan-out."
      />
      {withEntries.length > 0 && (
        <div className="pt-1">
          <div className="text-caption uppercase tracking-wider text-fg-subtle mb-1">With mappings</div>
          <div className="space-y-0.5">
            {withEntries.map((w, i) => (
              <div key={i} className="flex items-center gap-1 text-xs font-mono bg-surface-1 border border-border-default rounded px-1.5 py-0.5">
                <span className="text-fg-default">{w.key}</span>
                <span className="text-fg-subtle">=</span>
                <span className="text-fg-muted truncate" title={w.value}>{w.value}</span>
              </div>
            ))}
          </div>
          <p className="text-caption text-fg-subtle mt-1">Read-only — edit mappings in the source view.</p>
        </div>
      )}
      <div className="pt-2">
        <Button variant="secondary" size="sm" disabled={!decl.source} onClick={() => openChild(decl.source)}>
          Open child bot
        </Button>
      </div>
    </div>
  );
}

/** Read-only notice for a node that belongs to an expanded subbot's child
 *  graph — the node lives in ANOTHER file, so the inspector only names it
 *  and offers to open the child bot. */
function SubbotChildNotice({ subbotId, childId }: { subbotId: string; childId: string }) {
  const document = useDocumentStore((s) => s.document);
  const openChild = useOpenChildBot();
  const decl = (document?.subbots ?? []).find((sb) => sb.name === subbotId);
  const source = decl?.source ?? "";
  return (
    <div className="p-3 space-y-2">
      <div className="flex items-center gap-3 rounded-md border border-border-default bg-surface-1 px-3 py-3">
        <span className="text-xl">{NODE_ICONS.subbot}</span>
        <div className="min-w-0">
          <p className="text-sm font-semibold text-fg-default truncate">{childId}</p>
          <p className="text-xs text-fg-subtle mt-0.5">
            Belongs to {source || `subbot ${subbotId}`} (subbot {subbotId}) — read-only here.
          </p>
        </div>
      </div>
      <Button variant="secondary" size="sm" disabled={!source} onClick={() => openChild(source)}>
        Open {source || "child bot"}
      </Button>
    </div>
  );
}
