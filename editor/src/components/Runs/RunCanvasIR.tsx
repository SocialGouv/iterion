import { useEffect, useMemo, useState } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MarkerType,
  useReactFlow,
  type Edge as FlowEdge,
  type Node as FlowNode,
} from "@xyflow/react";

import type { ExecutionState, WireWorkflow } from "@/api/runs";
import { getRunWorkflow } from "@/api/runs";
import { autoLayout } from "@/lib/autoLayout";

import IRNode, { type AggregateStatus } from "./IRNode";

const nodeTypes = { ir: IRNode };

const ARROW = { type: MarkerType.ArrowClosed, width: 18, height: 18 } as const;

interface Props {
  runId: string;
  executions: ExecutionState[];
  selectedNodeId: string | null;
  onSelectNode: (id: string | null) => void;
}

// Aggregate the status of all executions sharing an ir_node_id.
// Priority is roughly "most attention-worthy first": running and
// failed dominate; finished/skipped only matter when nothing else is
// happening. Returns "none" when no execution touched this IR node.
function aggregateStatus(
  irNodeId: string,
  executions: ExecutionState[],
): { status: AggregateStatus; count: number } {
  let count = 0;
  let running = false;
  let failed = false;
  let paused = false;
  let finished = false;
  let skipped = false;
  for (const e of executions) {
    if (e.ir_node_id !== irNodeId) continue;
    count++;
    switch (e.status) {
      case "running":
        running = true;
        break;
      case "failed":
        failed = true;
        break;
      case "paused_waiting_human":
        paused = true;
        break;
      case "finished":
        finished = true;
        break;
      case "skipped":
        skipped = true;
        break;
    }
  }
  if (count === 0) return { status: "none", count: 0 };
  if (running) return { status: "running", count };
  if (failed && finished) return { status: "mixed", count };
  if (failed) return { status: "failed", count };
  if (paused) return { status: "paused_waiting_human", count };
  if (finished) return { status: "finished", count };
  if (skipped) return { status: "skipped", count };
  return { status: "none", count };
}

export default function RunCanvasIR({
  runId,
  executions,
  selectedNodeId,
  onSelectNode,
}: Props) {
  const [wf, setWf] = useState<WireWorkflow | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [nodes, setNodes] = useState<FlowNode[]>([]);
  const [edges, setEdges] = useState<FlowEdge[]>([]);
  const reactFlow = useReactFlow();

  // Fetch the IR projection once per runId. The endpoint re-compiles
  // the .iter source, so a tiny delay is expected; the empty-state UI
  // covers it.
  useEffect(() => {
    setWf(null);
    setError(null);
    let cancelled = false;
    getRunWorkflow(runId)
      .then((w) => {
        if (cancelled) return;
        setWf(w);
      })
      .catch((e) => {
        if (cancelled) return;
        setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, [runId]);

  // Aggregation depends on both the IR (which IDs exist) and the live
  // executions (which IDs were touched). Recompute whenever either
  // changes — usually cheap because executions list is small.
  const aggregates = useMemo(() => {
    if (!wf) return null;
    const out = new Map<string, { status: AggregateStatus; count: number }>();
    for (const n of wf.nodes) {
      out.set(n.id, aggregateStatus(n.id, executions));
    }
    return out;
  }, [wf, executions]);

  // Build base nodes/edges, then run ELK layout. Same pattern as
  // RunCanvas: relayout only when the structural set changes (which
  // here is "the IR" — a one-shot fetch — so effectively once).
  useEffect(() => {
    if (!wf || !aggregates) return;
    let cancelled = false;
    const baseNodes: FlowNode[] = wf.nodes.map((n) => {
      const agg = aggregates.get(n.id) ?? { status: "none" as AggregateStatus, count: 0 };
      return {
        id: n.id,
        type: "ir",
        position: { x: 0, y: 0 },
        data: {
          id: n.id,
          kind: n.kind,
          count: agg.count,
          status: agg.status,
          isEntry: n.id === wf.entry,
          selected: n.id === selectedNodeId,
        },
      };
    });
    const baseEdges: FlowEdge[] = wf.edges.map((e, i) => {
      const conditional = !!e.condition || !!e.expression;
      const isLoop = !!e.loop;
      const label =
        e.loop !== undefined && e.loop !== ""
          ? `loop ${e.loop}`
          : e.expression
          ? `expr`
          : e.condition
          ? `${e.negated ? "!" : ""}${e.condition}`
          : undefined;
      return {
        id: `ir-edge-${i}`,
        source: e.from,
        target: e.to,
        markerEnd: ARROW,
        animated: isLoop,
        label,
        labelStyle: { fontSize: 10 },
        labelBgStyle: { fill: "var(--surface-0, #fff)", opacity: 0.9 },
        labelBgPadding: [4, 2],
        style: conditional || isLoop ? { strokeDasharray: "4 3" } : undefined,
      };
    });

    autoLayout(baseNodes, baseEdges, "DOWN")
      .then((laid) => {
        if (cancelled) return;
        setNodes(laid);
        setEdges(baseEdges);
        requestAnimationFrame(() => {
          reactFlow.fitView({ padding: 0.2, duration: 250 });
        });
      })
      .catch(() => {
        if (cancelled) return;
        setNodes(baseNodes);
        setEdges(baseEdges);
      });
    return () => {
      cancelled = true;
    };
    // wf is the structural input; aggregates+selected are visual
    // patches handled below to avoid a layout reshuffle.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wf]);

  // Visual patch path: status flips and selection don't trigger ELK.
  useEffect(() => {
    if (!wf || !aggregates) return;
    setNodes((prev) =>
      prev.map((n) => {
        const agg = aggregates.get(n.id);
        if (!agg) return n;
        return {
          ...n,
          data: {
            ...(n.data as Record<string, unknown>),
            count: agg.count,
            status: agg.status,
            selected: n.id === selectedNodeId,
          },
        };
      }),
    );
  }, [wf, aggregates, selectedNodeId]);

  if (error) {
    return (
      <div className="h-full p-4 text-xs text-danger-fg">
        IR view unavailable: {error}
      </div>
    );
  }
  if (!wf) {
    return (
      <div className="h-full p-4 text-xs text-fg-subtle">Loading workflow…</div>
    );
  }

  return (
    <div className="h-full w-full">
      {wf.stale_hash && (
        <div className="absolute top-2 left-1/2 -translate-x-1/2 z-10 px-2 py-1 text-[10px] rounded bg-warning-soft text-warning-fg border border-warning/60 shadow">
          ⚠ The .iter source has changed since this run started; the
          structure shown may differ from what executed.
        </div>
      )}
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        fitView
        fitViewOptions={{ padding: 0.2 }}
        onNodeClick={(_e, n) => onSelectNode(n.id === selectedNodeId ? null : n.id)}
        onPaneClick={() => onSelectNode(null)}
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={16} size={1} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
