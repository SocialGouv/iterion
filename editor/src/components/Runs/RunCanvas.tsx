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

import type { ExecStatus, ExecutionState, RunEvent } from "@/api/runs";
import { autoLayout } from "@/lib/autoLayout";

import ExecutionNode from "./ExecutionNode";

const nodeTypes = { execution: ExecutionNode };

interface Props {
  executions: ExecutionState[];
  events: RunEvent[];
  selectedExecutionId: string | null;
  onSelect: (id: string | null) => void;
}

// Visual semantics: an edge's appearance reflects the state of its
// destination execution. Active = the runtime just took this edge and
// the target hasn't finished yet (animated, accent). Done = both ends
// terminal (solid, status-colored). Loop = backedge (perpetual
// animation with a distinct dash so it reads as recurring).
type EdgeState = "active" | "done-finished" | "done-failed" | "done-other" | "loop";

const ARROW = { type: MarkerType.ArrowClosed, width: 18, height: 18 } as const;

// Tailwind theme accents — stroke colors picked to be distinguishable
// against the canvas background and consistent with status pills used
// elsewhere in the UI. We render via inline style.stroke so React Flow
// edges pick them up without a custom edge component.
const COLOR_ACTIVE = "#3b82f6"; // blue-500: in-flight transition
const COLOR_FINISHED = "#22c55e"; // green-500: branch completed normally
const COLOR_FAILED = "#ef4444"; // red-500: branch failed/cancelled
const COLOR_NEUTRAL = "#a3a3a3"; // neutral-400: skipped/idle/unknown

function isTerminalStatus(s: ExecStatus | undefined): boolean {
  return s === "finished" || s === "failed" || s === "skipped";
}

function styleForState(state: EdgeState): {
  animated: boolean;
  style: React.CSSProperties;
  markerEnd: typeof ARROW & { color?: string };
} {
  switch (state) {
    case "active":
      return {
        animated: true,
        style: { stroke: COLOR_ACTIVE, strokeWidth: 2, strokeDasharray: "6 4" },
        markerEnd: { ...ARROW, color: COLOR_ACTIVE },
      };
    case "loop":
      return {
        animated: true,
        style: { stroke: COLOR_ACTIVE, strokeWidth: 1.5, strokeDasharray: "8 4" },
        markerEnd: { ...ARROW, color: COLOR_ACTIVE },
      };
    case "done-finished":
      return {
        animated: false,
        style: { stroke: COLOR_FINISHED, strokeWidth: 1.5 },
        markerEnd: { ...ARROW, color: COLOR_FINISHED },
      };
    case "done-failed":
      return {
        animated: false,
        style: { stroke: COLOR_FAILED, strokeWidth: 1.5 },
        markerEnd: { ...ARROW, color: COLOR_FAILED },
      };
    case "done-other":
    default:
      return {
        animated: false,
        style: { stroke: COLOR_NEUTRAL, strokeWidth: 1.5 },
        markerEnd: { ...ARROW, color: COLOR_NEUTRAL },
      };
  }
}

/** Build flow edges by walking the event stream. We use the engine's
 *  emitted `edge_selected` events (which carry the resolved IR-level
 *  `from`/`to` plus loop/condition metadata) to know exactly which
 *  edge fired, then pair each one with its destination's freshly
 *  minted execution_id by waiting for the subsequent node_started.
 *
 *  This is more reliable than purely inferring from
 *  node_finished → node_started ordering: it handles loop edges
 *  correctly (matching iteration counters), labels conditional
 *  edges with their condition string, and survives skipped nodes
 *  (e.g. routers that finish without producing an output the next
 *  node consumes). */
interface RawEdge {
  id: string;
  source: string;
  target: string;
  condition?: string;
  loop?: string;
  iteration?: number;
}

function deriveEdges(events: RunEvent[], executions: ExecutionState[]): FlowEdge[] {
  const raw: RawEdge[] = [];
  const seen = new Set<string>();

  // Track per-(branch, node) the iteration count seen so far so we
  // can mint matching execution_ids on the destination side.
  const startedCounts = new Map<string, number>();
  // Per-branch map of IR node_id → most recent execution_id that
  // has finished. Source of `from` resolution.
  const lastFinishedExec = new Map<string, Map<string, string>>();
  // Per-branch queue of edges that fired but are waiting for the
  // matching node_started to know the destination's iteration.
  const pending = new Map<
    string,
    Array<{
      fromNode: string;
      toNode: string;
      condition?: string;
      loop?: string;
      iteration?: number;
    }>
  >();

  const branchKey = (b: string | undefined) => b || "main";

  const lastFinishedFor = (branch: string, node: string): string | undefined => {
    return lastFinishedExec.get(branch)?.get(node);
  };

  const setLastFinished = (branch: string, node: string, execId: string) => {
    let m = lastFinishedExec.get(branch);
    if (!m) {
      m = new Map();
      lastFinishedExec.set(branch, m);
    }
    m.set(node, execId);
  };

  for (const e of events) {
    const branch = branchKey(e.branch_id);

    switch (e.type) {
      case "edge_selected": {
        const from = (e.data?.from as string | undefined) ?? "";
        const to = (e.data?.to as string | undefined) ?? "";
        if (!from || !to) break;
        let q = pending.get(branch);
        if (!q) {
          q = [];
          pending.set(branch, q);
        }
        q.push({
          fromNode: from,
          toNode: to,
          condition: e.data?.condition as string | undefined,
          loop: e.data?.loop as string | undefined,
          iteration:
            typeof e.data?.iteration === "number"
              ? (e.data.iteration as number)
              : undefined,
        });
        break;
      }

      case "node_started": {
        if (!e.node_id) break;
        const key = `${branch} ${e.node_id}`;
        const iter = startedCounts.get(key) ?? 0;
        startedCounts.set(key, iter + 1);
        const dst = `exec:${branch}:${e.node_id}:${iter}`;

        // Try to consume a pending edge_selected whose `to` matches.
        const q = pending.get(branch);
        if (q) {
          const idx = q.findIndex((p) => p.toNode === e.node_id);
          if (idx >= 0) {
            const edge = q[idx];
            q.splice(idx, 1);
            if (edge) {
              const src = lastFinishedFor(branch, edge.fromNode);
              if (src) {
                const id = `${src}->${dst}`;
                if (!seen.has(id)) {
                  seen.add(id);
                  raw.push({
                    id,
                    source: src,
                    target: dst,
                    condition: edge.condition,
                    loop: edge.loop,
                    iteration: edge.iteration,
                  });
                }
              }
            }
          }
        }
        // Fallback: if no pending edge matched (e.g. the entry node)
        // we still try the "previous finished on this branch" rule.
        // This catches branch_started → first_node transitions where
        // edge_selected is emitted on a different branch.
        break;
      }

      case "node_finished": {
        if (!e.node_id) break;
        const key = `${branch} ${e.node_id}`;
        const iter = (startedCounts.get(key) ?? 1) - 1;
        const id = `exec:${branch}:${e.node_id}:${iter}`;
        setLastFinished(branch, e.node_id, id);
        break;
      }

      default:
        break;
    }
  }

  // Resolve visual state for each edge by inspecting source+target
  // statuses. Done after the event walk so we can use the latest
  // ExecutionState (which the run store keeps in sync via reducers
  // that fire after every event).
  const execById = new Map<string, ExecutionState>();
  for (const ex of executions) execById.set(ex.execution_id, ex);

  return raw.map<FlowEdge>((e) => {
    const dst = execById.get(e.target);
    const dstStatus = dst?.status;

    let state: EdgeState;
    if (e.loop) {
      // Loop backedges: keep them visually "alive" as long as the
      // loop body is still running. Once the body terminates, fade
      // to the resolved color (the loop_check guard's status).
      state = isTerminalStatus(dstStatus) ? "done-other" : "loop";
    } else if (!dst || !isTerminalStatus(dstStatus)) {
      state = "active";
    } else if (dstStatus === "finished") {
      state = "done-finished";
    } else if (dstStatus === "failed") {
      state = "done-failed";
    } else {
      state = "done-other";
    }

    const visual = styleForState(state);

    // Loops show "loop counter_loop iter N/?" so the user can tell at
    // a glance which iteration the runtime is on. Conditionals show
    // their condition string. Pure sequential transitions are
    // unlabeled to keep the canvas clean.
    let label: string | undefined;
    if (e.loop) {
      const iterPart = e.iteration !== undefined ? ` iter ${e.iteration + 1}` : "";
      label = `loop ${e.loop}${iterPart}`;
    } else if (e.condition) {
      label = e.condition;
    }

    return {
      id: e.id,
      source: e.source,
      target: e.target,
      animated: visual.animated,
      label,
      style: visual.style,
      markerEnd: visual.markerEnd,
      // Slightly lift the loop label so it doesn't collide with the
      // sequential edge that goes the other way.
      labelStyle: e.loop ? { fontSize: 10, fontStyle: "italic" } : { fontSize: 10 },
      labelBgStyle: { fill: "var(--surface-0, #fff)", opacity: 0.9 },
      labelBgPadding: [4, 2],
      // Keep src/dst on the edge for downstream debugging — also helps
      // React Flow's diffing avoid full re-renders when only the visual
      // state changes (animation flip on status change).
      data: { source: e.source, target: e.target },
    };
  });
}

export default function RunCanvas({
  executions,
  events,
  selectedExecutionId,
  onSelect,
}: Props) {
  const [nodes, setNodes] = useState<FlowNode[]>([]);
  const [edges, setEdges] = useState<FlowEdge[]>([]);
  const reactFlow = useReactFlow();

  // Re-layout when the execution count changes — but NOT on every
  // status flip, otherwise the whole graph reshuffles every time a
  // node finishes. Drives ELK only when the structural set changes.
  const execIdsKey = useMemo(
    () => executions.map((e) => e.execution_id).join("|"),
    [executions],
  );

  useEffect(() => {
    let cancelled = false;
    const baseNodes: FlowNode[] = executions.map((exec) => ({
      id: exec.execution_id,
      type: "execution",
      position: { x: 0, y: 0 },
      data: { exec, selected: exec.execution_id === selectedExecutionId },
    }));
    const baseEdges = deriveEdges(events, executions);

    autoLayout(baseNodes, baseEdges, "DOWN")
      .then((laid) => {
        if (cancelled) return;
        setNodes(laid);
        setEdges(baseEdges);
        // Pan/zoom to include the freshly-grown graph. Without this,
        // new nodes appear off-screen as the run progresses and the
        // user has to manually pan to find them.
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
    // execIdsKey ensures we relayout only when the set of executions
    // changes; events length covers edge derivation drift (new
    // edge_selected events may arrive without new executions).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [execIdsKey, events.length]);

  // Cheaper update path: status flips re-render existing nodes by
  // patching their data without re-layout. Edges also need to react
  // (color/animation changes when target moves to a terminal status)
  // so we recompute deriveEdges with the current executions snapshot
  // — cheap because deriveEdges is O(events) and edges array is
  // tiny. We skip autoLayout here: ReactFlow re-renders edges in
  // place against the existing node positions.
  useEffect(() => {
    setNodes((prev) =>
      prev.map((n) => {
        const exec = executions.find((e) => e.execution_id === n.id);
        if (!exec) return n;
        return {
          ...n,
          data: { exec, selected: exec.execution_id === selectedExecutionId },
        };
      }),
    );
    setEdges(deriveEdges(events, executions));
  }, [executions, selectedExecutionId, events]);

  return (
    <div className="h-full w-full">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        fitView
        fitViewOptions={{ padding: 0.2 }}
        onNodeClick={(_e, n) => onSelect(n.id === selectedExecutionId ? null : n.id)}
        onPaneClick={() => onSelect(null)}
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={16} size={1} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
