import { useCallback, useRef, useState } from "react";
import type { Connection, Edge as FlowEdge } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { findNodeDecl } from "@/lib/defaults";
import { assignFieldToNode, addToolToNode } from "@/lib/docMutations";
import { TOAST_DURATION_CONNECTION_ERROR_MS } from "@/lib/constants";
import { parseDetailId } from "@/lib/nodeDetailGraph";
import type { IterDocument } from "@/api/types";

interface QuickAddState {
  x: number;
  y: number;
  sourceId: string;
}

function handleSubNodeConnect(
  sourceId: string,
  targetId: string,
  centralNodeId: string,
  doc: IterDocument,
  applyBatch: (mutator: (doc: IterDocument) => IterDocument) => void,
  addToast: (message: string, type: "success" | "error" | "info" | "warning") => void,
): void {
  const src = parseDetailId(sourceId);
  const tgt = parseDetailId(targetId);
  if (!src || !tgt) { addToast("Invalid connection", "error"); return; }

  const found = findNodeDecl(doc, centralNodeId);
  if (!found) return;

  // Normalize direction: extract the non-central side and its info
  const sub = src.kind === "central" ? tgt : src;
  const isCentralInvolved = src.kind === "central" || tgt.kind === "central";

  // Schema <-> Central: assign schema to input/output slot
  if (isCentralInvolved && sub.kind === "schema" && sub.name) {
    const defaultRole = src.kind === "central" ? "output" : "input";
    const role = sub.relation ?? defaultRole;
    applyBatch((d) => assignFieldToNode(d, centralNodeId, role, sub.name!));
    addToast(`Schema "${sub.name}" assigned as ${role}`, "success");
    return;
  }

  // Prompt <-> Central: assign prompt to slot
  if (isCentralInvolved && sub.kind === "prompt" && sub.name) {
    const relation = sub.relation;
    if (!relation) { addToast("Cannot determine prompt slot", "error"); return; }
    applyBatch((d) => assignFieldToNode(d, centralNodeId, relation, sub.name!));
    addToast(`Prompt "${sub.name}" assigned as ${relation}`, "success");
    return;
  }

  // Tool <-> Central: add tool to agent/judge
  if (isCentralInvolved && sub.kind === "tool" && sub.name) {
    if (found.kind !== "agent" && found.kind !== "judge") {
      addToast("Only agents and judges can have tools", "error");
      return;
    }
    applyBatch((d) => addToolToNode(d, centralNodeId, sub.name!));
    addToast(`Tool "${sub.name}" added`, "success");
    return;
  }

  // Var -> Prompt: insert {{vars.NAME}} reference
  if (src.kind === "var" && tgt.kind === "prompt" && src.name && tgt.name) {
    const ref = `{{vars.${src.name}}}`;
    applyBatch((d) => ({
      ...d,
      prompts: d.prompts.map((p) => {
        if (p.name !== tgt.name) return p;
        if (p.body.includes(ref)) return p;
        return { ...p, body: p.body ? p.body + "\n" + ref : ref };
      }),
    }));
    addToast(`Variable reference ${ref} added to prompt "${tgt.name}"`, "success");
    return;
  }

  addToast("Invalid connection in detail view", "error");
}

export function useCanvasConnections() {
  const document = useDocumentStore((s) => s.document);
  const addEdge = useDocumentStore((s) => s.addEdge);
  const applyBatch = useDocumentStore((s) => s.applyBatch);
  const activeWorkflow = useActiveWorkflow();
  const addToast = useUIStore((s) => s.addToast);

  const [connectionError, setConnectionError] = useState<string | null>(null);
  const connectionErrorTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const [quickAddMenu, setQuickAddMenu] = useState<QuickAddState | null>(null);
  const pendingConnectSourceRef = useRef<string | null>(null);
  const edgeCountBeforeConnectRef = useRef<number>(0);

  const getConnectionError = useCallback(
    (connection: FlowEdge | Connection): string | null => {
      if (!connection.source || !connection.target) return "Invalid connection";
      if (connection.source === connection.target) return "Cannot connect a node to itself";
      if (connection.source === "done" || connection.source === "fail") return "Cannot connect from a terminal node";
      if (connection.source === "__start__" || connection.target === "__start__") return "Cannot connect to the start node";
      if (activeWorkflow) {
        const dup = activeWorkflow.edges.some(
          (e) => e.from === connection.source && e.to === connection.target,
        );
        if (dup) return "This connection already exists";
      }
      return null;
    },
    [activeWorkflow],
  );

  const isValidConnection = useCallback(
    (connection: FlowEdge | Connection) => {
      // In subnode view, allow any connection between detail nodes
      const subNodeViewStack = useUIStore.getState().subNodeViewStack;
      if (subNodeViewStack.length > 0) {
        if (!connection.source || !connection.target) return false;
        if (connection.source === connection.target) return false;
        const src = parseDetailId(connection.source);
        const tgt = parseDetailId(connection.target);
        if (!src || !tgt) return false;
        // Valid combos: schema<->central, prompt<->central, var->prompt, tool<->central
        if ((src.kind === "schema" && tgt.kind === "central") || (src.kind === "central" && tgt.kind === "schema")) return true;
        if ((src.kind === "prompt" && tgt.kind === "central") || (src.kind === "central" && tgt.kind === "prompt")) return true;
        if (src.kind === "var" && tgt.kind === "prompt") return true;
        if ((src.kind === "tool" && tgt.kind === "central") || (src.kind === "central" && tgt.kind === "tool")) return true;
        return false;
      }
      return getConnectionError(connection) === null;
    },
    [getConnectionError],
  );

  const onConnect = useCallback(
    (connection: Connection) => {
      if (!document || !connection.source || !connection.target) return;

      // Subnode view: semantic connections
      const subNodeViewStack = useUIStore.getState().subNodeViewStack;
      if (subNodeViewStack.length > 0) {
        const centralNodeId = subNodeViewStack[subNodeViewStack.length - 1]!;
        handleSubNodeConnect(connection.source, connection.target, centralNodeId, document, applyBatch, addToast);
        return;
      }

      const workflowName = activeWorkflow?.name;
      if (!workflowName) return;
      const error = getConnectionError(connection);
      if (error) {
        setConnectionError(error);
        clearTimeout(connectionErrorTimer.current);
        connectionErrorTimer.current = setTimeout(() => setConnectionError(null), TOAST_DURATION_CONNECTION_ERROR_MS);
        return;
      }
      addEdge(workflowName, { from: connection.source, to: connection.target });
    },
    [document, activeWorkflow, addEdge, applyBatch, addToast, getConnectionError],
  );

  const onConnectStart = useCallback(
    (_event: unknown, params: { nodeId: string | null }) => {
      setConnectionError(null);
      pendingConnectSourceRef.current = params.nodeId;
      edgeCountBeforeConnectRef.current = activeWorkflow?.edges?.length ?? 0;
    },
    [activeWorkflow],
  );

  const onConnectEnd = useCallback(
    (event: MouseEvent | TouchEvent) => {
      const source = pendingConnectSourceRef.current;
      pendingConnectSourceRef.current = null;

      if (!source || !document || !activeWorkflow) return;

      // Read fresh state to avoid stale closure after addEdge in onConnect
      const freshDoc = useDocumentStore.getState().document;
      const freshWorkflow = freshDoc?.workflows.find((w) => w.name === activeWorkflow.name);
      const currentEdgeCount = freshWorkflow?.edges?.length ?? 0;
      if (currentEdgeCount > edgeCountBeforeConnectRef.current) return;

      // No edge created — show quick-add menu at the drop position
      const clientX = "clientX" in event ? event.clientX : event.changedTouches?.[0]?.clientX ?? 0;
      const clientY = "clientY" in event ? event.clientY : event.changedTouches?.[0]?.clientY ?? 0;

      // Only show if drop was on the canvas (not on a node)
      const target = event.target as HTMLElement;
      if (target.closest(".react-flow__node")) return;

      setQuickAddMenu({ x: clientX, y: clientY, sourceId: source });
    },
    [document, activeWorkflow],
  );

  return {
    connectionError,
    quickAddMenu,
    setQuickAddMenu,
    isValidConnection,
    onConnect,
    onConnectStart,
    onConnectEnd,
    addEdge,
  };
}
