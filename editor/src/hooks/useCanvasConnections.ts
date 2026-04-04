import { useCallback, useRef, useState } from "react";
import type { Connection, Edge as FlowEdge } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { TOAST_DURATION_CONNECTION_ERROR_MS } from "@/lib/constants";

interface QuickAddState {
  x: number;
  y: number;
  sourceId: string;
}

export function useCanvasConnections() {
  const document = useDocumentStore((s) => s.document);
  const addEdge = useDocumentStore((s) => s.addEdge);
  const activeWorkflow = useActiveWorkflow();

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
    (connection: FlowEdge | Connection) => getConnectionError(connection) === null,
    [getConnectionError],
  );

  const onConnect = useCallback(
    (connection: Connection) => {
      if (!document || !connection.source || !connection.target) return;
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
    [document, activeWorkflow, addEdge, getConnectionError],
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
