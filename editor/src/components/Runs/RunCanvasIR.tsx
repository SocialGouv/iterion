import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MarkerType,
  useReactFlow,
  type Edge as FlowEdge,
  type Node as FlowNode,
} from "@xyflow/react";

import type { ExecStatus, ExecutionState, WireWorkflow } from "@/api/runs";
import { getRunWorkflow } from "@/api/runs";
import { autoLayout } from "@/lib/autoLayout";

import IRNode, { iterationColor } from "./IRNode";

const nodeTypes = { ir: IRNode };

const ARROW = { type: MarkerType.ArrowClosed, width: 18, height: 18 } as const;

interface Props {
  runId: string;
  executions: ExecutionState[];
  selectedNodeId: string | null;
  onSelectNode: (id: string | null) => void;
  // Per-IR-node iteration selection. Owned by the parent so the
  // detail panel can resolve which exec to render. Default is
  // computed from `executions` (current > paused > latest).
  iterationByNode: Map<string, number>;
  onSelectIteration: (nodeId: string, iteration: number) => void;
}

function nodeMatchesFilters(
  execs: ExecutionState[],
  filters: Set<StatusFilter>,
): boolean {
  if (filters.size === 0) return true;
  const want: Record<StatusFilter, ExecStatus> = {
    running: "running",
    paused: "paused_waiting_human",
    failed: "failed",
  };
  for (const f of filters) {
    if (execs.some((e) => e.status === want[f])) return true;
  }
  return false;
}

// Compute the "current" iteration for an IR node — the one we want to
// land on when the user first opens the run console. Priority is the
// in-flight iteration first, then a paused one, then the most recent
// finished. Returns 0 when there are no executions yet.
export function defaultIterationFor(execs: ExecutionState[]): number {
  if (execs.length === 0) return 0;
  // Index iterations so we can scan once and return the most relevant.
  let running: number | undefined;
  let paused: number | undefined;
  let maxIter = 0;
  for (const e of execs) {
    if (e.status === "running" && running === undefined) running = e.loop_iteration;
    if (e.status === "paused_waiting_human" && paused === undefined) paused = e.loop_iteration;
    if (e.loop_iteration > maxIter) maxIter = e.loop_iteration;
  }
  if (running !== undefined) return running;
  if (paused !== undefined) return paused;
  return maxIter;
}

type StatusFilter = "running" | "paused" | "failed";

export default function RunCanvasIR({
  runId,
  executions,
  selectedNodeId,
  onSelectNode,
  iterationByNode,
  onSelectIteration,
}: Props) {
  const [wf, setWf] = useState<WireWorkflow | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [nodes, setNodes] = useState<FlowNode[]>([]);
  const [edges, setEdges] = useState<FlowEdge[]>([]);
  const [activeFilters, setActiveFilters] = useState<Set<StatusFilter>>(
    () => new Set(),
  );
  const reactFlow = useReactFlow();

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

  // Group executions by IR node id once; both the layout and the
  // visual-patch effects below reuse this.
  const execsByNode = useMemo(() => {
    const m = new Map<string, ExecutionState[]>();
    for (const ex of executions) {
      const list = m.get(ex.ir_node_id);
      if (list) list.push(ex);
      else m.set(ex.ir_node_id, [ex]);
    }
    // Keep iteration order so the timeline pips render left-to-right.
    for (const list of m.values()) {
      list.sort((a, b) => a.loop_iteration - b.loop_iteration);
    }
    return m;
  }, [executions]);

  const handleSelectIteration = useCallback(
    (nodeId: string, iteration: number) => {
      onSelectIteration(nodeId, iteration);
      // Also select the node so the detail panel follows the picked
      // iteration without an extra click.
      onSelectNode(nodeId);
    },
    [onSelectIteration, onSelectNode],
  );

  // Layout pass — runs once when the IR arrives. Iteration changes
  // and execution flips are handled by the patch effect below.
  useEffect(() => {
    if (!wf) return;
    let cancelled = false;
    const baseNodes: FlowNode[] = wf.nodes.map((n) => {
      const execs = execsByNode.get(n.id) ?? [];
      const selectedIteration =
        iterationByNode.get(n.id) ?? defaultIterationFor(execs);
      return {
        id: n.id,
        type: "ir",
        position: { x: 0, y: 0 },
        data: {
          id: n.id,
          kind: n.kind,
          executions: execs,
          selectedIteration,
          isEntry: n.id === wf.entry,
          selected: n.id === selectedNodeId,
          onSelectIteration: handleSelectIteration,
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
      // Loop backedges get the iteration-palette color so the eye can
      // associate them with the matching node-pip color when scanning
      // the canvas. Other edges stay neutral.
      const lastIter = (execsByNode.get(e.from)?.length ?? 0) - 1;
      const loopStroke = isLoop ? iterationColor(Math.max(lastIter, 0)) : undefined;
      return {
        id: `ir-edge-${i}`,
        source: e.from,
        target: e.to,
        markerEnd: loopStroke ? { ...ARROW, color: loopStroke } : ARROW,
        animated: isLoop,
        label,
        labelStyle: { fontSize: 10 },
        labelBgStyle: { fill: "var(--surface-0, #fff)", opacity: 0.9 },
        labelBgPadding: [4, 2],
        style:
          isLoop
            ? { strokeDasharray: "8 4", stroke: loopStroke }
            : conditional
            ? { strokeDasharray: "4 3" }
            : undefined,
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
    // wf is the only structural input. iterationByNode/executions are
    // refreshed by the patch effect below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wf]);

  // Visual patch: rerun whenever executions, selection, or per-node
  // iteration changes. Cheap because it only mutates `data` — no
  // ELK relayout. Skipped when the layout effect hasn't completed.
  useEffect(() => {
    if (!wf) return;
    setNodes((prev) =>
      prev.map((n) => {
        const execs = execsByNode.get(n.id) ?? [];
        const selectedIteration =
          iterationByNode.get(n.id) ?? defaultIterationFor(execs);
        const dimmed =
          activeFilters.size > 0 && !nodeMatchesFilters(execs, activeFilters);
        return {
          ...n,
          data: {
            ...(n.data as Record<string, unknown>),
            executions: execs,
            selectedIteration,
            selected: n.id === selectedNodeId,
            onSelectIteration: handleSelectIteration,
          },
          style: dimmed ? { opacity: 0.25 } : undefined,
        };
      }),
    );
  }, [
    wf,
    execsByNode,
    iterationByNode,
    selectedNodeId,
    handleSelectIteration,
    activeFilters,
  ]);

  // When the user invokes "jump to failed" (or any other external
  // navigation), centre the canvas on the selected node. Only triggers
  // when the selection actually exists in the layout — otherwise the
  // initial fitView (run open) is what handles it.
  useEffect(() => {
    if (!selectedNodeId) return;
    const node = nodes.find((n) => n.id === selectedNodeId);
    if (!node) return;
    reactFlow.setCenter(
      node.position.x + 100,
      node.position.y + 40,
      { zoom: 1, duration: 350 },
    );
  }, [selectedNodeId, nodes, reactFlow]);

  const filterCounts = useMemo(() => {
    let running = 0,
      paused = 0,
      failed = 0;
    for (const execs of execsByNode.values()) {
      for (const ex of execs) {
        if (ex.status === "running") running += 1;
        if (ex.status === "paused_waiting_human") paused += 1;
        if (ex.status === "failed") failed += 1;
      }
    }
    return { running, paused, failed };
  }, [execsByNode]);

  const toggleFilter = (f: StatusFilter) => {
    setActiveFilters((prev) => {
      const next = new Set(prev);
      if (next.has(f)) next.delete(f);
      else next.add(f);
      return next;
    });
  };

  if (error) {
    return (
      <div className="h-full p-4 text-xs text-danger-fg">
        Workflow view unavailable: {error}
      </div>
    );
  }
  if (!wf) {
    return (
      <div className="h-full p-4 text-xs text-fg-subtle">Loading workflow…</div>
    );
  }

  const filterChips: Array<{ key: StatusFilter; label: string; count: number; tone: string }> =
    [
      {
        key: "failed",
        label: "Failed",
        count: filterCounts.failed,
        tone: "text-danger-fg border-danger/40",
      },
      {
        key: "running",
        label: "Running",
        count: filterCounts.running,
        tone: "text-info-fg border-info/40",
      },
      {
        key: "paused",
        label: "Paused",
        count: filterCounts.paused,
        tone: "text-warning-fg border-warning/40",
      },
    ];

  return (
    <div className="h-full w-full relative">
      {wf.stale_hash && (
        <div className="absolute top-2 left-1/2 -translate-x-1/2 z-10 px-2 py-1 text-[10px] rounded bg-warning-soft text-warning-fg border border-warning/60 shadow">
          ⚠ The .iter source has changed since this run started; the
          structure shown may differ from what executed.
        </div>
      )}
      <div className="absolute top-2 right-2 z-10 flex items-center gap-1">
        {filterChips
          .filter((c) => c.count > 0)
          .map((c) => {
            const isActive = activeFilters.has(c.key);
            return (
              <button
                key={c.key}
                type="button"
                onClick={() => toggleFilter(c.key)}
                className={`text-[10px] px-2 py-0.5 rounded border transition-colors bg-surface-1/90 backdrop-blur ${
                  c.tone
                } ${
                  isActive
                    ? "ring-1 ring-accent bg-surface-2"
                    : "hover:bg-surface-2"
                }`}
                title={
                  isActive
                    ? `Stop highlighting ${c.label.toLowerCase()} nodes`
                    : `Highlight ${c.label.toLowerCase()} nodes`
                }
              >
                {c.label} <span className="font-mono">{c.count}</span>
              </button>
            );
          })}
      </div>
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
