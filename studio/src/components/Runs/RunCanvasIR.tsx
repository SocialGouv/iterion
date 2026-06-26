import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MarkerType,
  useReactFlow,
  type Edge as FlowEdge,
  type Node as FlowNode,
} from "@xyflow/react";

import type { ExecutionState, WireNode } from "@/api/runs";
import { autoLayout } from "@/lib/autoLayout";
import type { DelegateOutputMeta } from "@/lib/delegateMeta";
import { useToggleSet } from "@/hooks/useToggleSet";

import { useUIStore } from "@/store/ui";
import { useThemeStore } from "@/store/theme";

import IRNode, { iterationColor } from "./IRNode";
import FanoutFrame from "./FanoutFrame";
import RunCanvasToolbar from "./RunCanvasToolbar";
import { FilterChips, buildFilterChips } from "./runCanvasIR/FilterChips";
import { StatusLegend } from "./runCanvasIR/StatusLegend";
import {
  buildLLMMeta,
  defaultIterationFor,
  nodeMatchesFilters,
  type StatusFilter,
} from "./runCanvasIR/helpers";
import { useEffortCapsPrefetch } from "./runCanvasIR/useEffortCapsPrefetch";
import { useInitialRunningFocus } from "./runCanvasIR/useInitialRunningFocus";
import { useSelectedNodeFocus } from "./runCanvasIR/useSelectedNodeFocus";
import { useWorkflowLoad } from "./runCanvasIR/useWorkflowLoad";

// Re-export so existing `import { defaultIterationFor } from
// "./RunCanvasIR"` callers (RunView's selectedNodeIteration memo) keep
// resolving without churn.
export { defaultIterationFor };

const nodeTypes = { ir: IRNode, fanoutFrame: FanoutFrame };

// Padding (px) added around the bounding box of a fan-out region when
// drawing its frame, plus headroom at the top for the floating label.
const FANOUT_FRAME_PAD = 26;
const FANOUT_FRAME_HEADER = 16;
// Real rendered IRNode footprint. ELK lays out with NODE_WIDTH/NODE_HEIGHT
// (160×80), but the actual card is `w-[200px]` and taller once the
// status/iteration-pip rows render — so the bbox must use these larger
// values or wide/tall nodes poke out of the frame's right/bottom edge.
const FANOUT_NODE_W = 200;
const FANOUT_NODE_H = 120;

// Derive fan_out_each replicated regions purely from run executions.
// branch_id is `branch_<routerNodeID>_<itemIdx>` (see runtime
// fan_out_each.go). For each node we count the distinct items that
// executed it (→ the multi-instance badge) and group nodes by their
// router (→ the region frame). Nodes that only ever ran on "main"
// (start/router/join/done) are absent here and stay un-framed.
function computeFanout(executions: ExecutionState[]) {
  const re = /^branch_(.+)_(\d+)$/;
  const routerItems = new Map<string, Set<string>>();
  const nodeRouterIdx = new Map<string, Map<string, Set<string>>>();
  for (const ex of executions) {
    const m = re.exec(ex.branch_id || "");
    if (!m) continue;
    const router = m[1]!;
    const idx = m[2]!;
    let ri = routerItems.get(router);
    if (!ri) routerItems.set(router, (ri = new Set()));
    ri.add(idx);
    let nr = nodeRouterIdx.get(ex.ir_node_id);
    if (!nr) nodeRouterIdx.set(ex.ir_node_id, (nr = new Map()));
    let s = nr.get(router);
    if (!s) nr.set(router, (s = new Set()));
    s.add(idx);
  }
  const replicationByNode = new Map<
    string,
    { count: number; total: number; router: string }
  >();
  const regionNodesByRouter = new Map<string, Set<string>>();
  for (const [nodeId, nr] of nodeRouterIdx) {
    let best: { router: string; count: number } | null = null;
    for (const [router, idxs] of nr) {
      if (!best || idxs.size > best.count) best = { router, count: idxs.size };
    }
    if (!best) continue;
    const total = routerItems.get(best.router)?.size ?? best.count;
    replicationByNode.set(nodeId, { count: best.count, total, router: best.router });
    let rn = regionNodesByRouter.get(best.router);
    if (!rn) regionNodesByRouter.set(best.router, (rn = new Set()));
    rn.add(nodeId);
  }
  return { replicationByNode, regionNodesByRouter, routerItems };
}

// Build the synthetic frame nodes for each fan-out region from laid-out
// positions (ELK uses fixed NODE_WIDTH×NODE_HEIGHT). Drawn behind the real
// nodes (prepended + zIndex −1, pointer-events none).
function buildFanoutFrames(
  laid: FlowNode[],
  regionNodesByRouter: Map<string, Set<string>>,
  routerItems: Map<string, Set<string>>,
): FlowNode[] {
  const posById = new Map(laid.map((n) => [n.id, n.position]));
  const frames: FlowNode[] = [];
  for (const [router, regionNodes] of regionNodesByRouter) {
    let minX = Infinity,
      minY = Infinity,
      maxX = -Infinity,
      maxY = -Infinity;
    for (const id of regionNodes) {
      const p = posById.get(id);
      if (!p) continue;
      minX = Math.min(minX, p.x);
      minY = Math.min(minY, p.y);
      maxX = Math.max(maxX, p.x + FANOUT_NODE_W);
      maxY = Math.max(maxY, p.y + FANOUT_NODE_H);
    }
    if (!isFinite(minX)) continue;
    frames.push({
      id: `__fanout__${router}`,
      type: "fanoutFrame",
      position: { x: minX - FANOUT_FRAME_PAD, y: minY - FANOUT_FRAME_PAD - FANOUT_FRAME_HEADER },
      draggable: false,
      selectable: false,
      zIndex: -1,
      data: {
        width: maxX - minX + FANOUT_FRAME_PAD * 2,
        height: maxY - minY + FANOUT_FRAME_PAD * 2 + FANOUT_FRAME_HEADER,
        label: `fan_out_each · ${router}`,
        total: routerItems.get(router)?.size ?? 0,
      },
    } as FlowNode);
  }
  return frames;
}

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
  // Latest runtime meta observed in llm_request / node_finished events,
  // keyed by IR node id. Empty before any LLM call has happened.
  // Populated by RunView's fold (see DelegateOutputMeta for the shape
  // and which event sources feed which field).
  runtimeOverrideByNode: Map<string, DelegateOutputMeta>;
  // Follow-live state surfaced in the canvas toolbar so the user
  // can see whether selection auto-tracks the running node and
  // toggle it without opening the (often-collapsed) detail panel.
  followLive: boolean;
  onToggleFollowLive: () => void;
}

export default function RunCanvasIR({
  runId,
  executions,
  selectedNodeId,
  onSelectNode,
  iterationByNode,
  onSelectIteration,
  runtimeOverrideByNode,
  followLive,
  onToggleFollowLive,
}: Props) {
  const { wf, error } = useWorkflowLoad(runId);
  const [nodes, setNodes] = useState<FlowNode[]>([]);
  const [edges, setEdges] = useState<FlowEdge[]>([]);
  // Bumped each time ELK layout settles so the centering effect below
  // can re-fire once `nodes` actually exists — the existing
  // [selectedNodeId] dep alone misses the entry race where selectedNodeId
  // is set before the IR fetch + autoLayout complete (nodes=[] → silent
  // exit, and the next dep change is too late).
  const [layoutEpoch, setLayoutEpoch] = useState(0);
  const { set: activeFilters, toggle: toggleFilter } = useToggleSet<StatusFilter>();
  // Effort capabilities (supported levels + default) keyed by
  // `${backend} ${model}`. Populated once the workflow lands by
  // walking unique pairs and asking /api/effort-capabilities.
  // buildLLMMeta uses `default` to render an attenuated badge when
  // the workflow declares no effort, and `supported` to normalise the
  // bar fill so a model's max always renders fully.
  const effortCapsByPair = useEffortCapsPrefetch(wf);
  // Ref mirror of effortCapsByPair so the async layout effect can read
  // the latest caps when its autoLayout promise resolves. Without this,
  // a fetch that completes mid-layout produces stale meta on first
  // paint (the layout's setNodes overwrites the patch effect's update).
  const effortCapsByPairRef = useRef(effortCapsByPair);
  useEffect(() => {
    effortCapsByPairRef.current = effortCapsByPair;
  }, [effortCapsByPair]);
  // Mirrors of the data inputs used inside the async ELK .then() so
  // a layout that takes ~50–200ms doesn't commit stale executions /
  // selection / runtime meta on top of the patch effect's update.
  // The patch effect (below) already deps on these directly; these
  // refs are read only by the .then() callback so it sees the values
  // that exist when the promise resolves, not when the effect fired.
  const execsByNodeRef = useRef<Map<string, ExecutionState[]>>(new Map());
  const iterationByNodeRef = useRef(iterationByNode);
  const runtimeOverrideByNodeRef = useRef(runtimeOverrideByNode);
  const selectedNodeIdRef = useRef(selectedNodeId);
  const reactFlow = useReactFlow();
  // Shared with the studio canvas so the user's TB/LR preference
  // persists across views; the toggle button in RunCanvasToolbar
  // flips this and the layout effect below picks it up.
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const toggleLayoutDirection = useUIStore((s) => s.toggleLayoutDirection);
  // Match the editor Canvas so the run graph follows light/dark instead of
  // React Flow's default light colorMode (which left light controls/legend
  // stranded on the dark run console).
  const resolvedTheme = useThemeStore((s) => s.resolved);

  // Group executions by IR node id once; both the layout and the
  // visual-patch effects below reuse this.
  const execsByNode = useMemo(() => {
    const m = new Map<string, ExecutionState[]>();
    for (const ex of executions) {
      const list = m.get(ex.ir_node_id);
      if (list) list.push(ex);
      else m.set(ex.ir_node_id, [ex]);
    }
    // Order pips left-to-right by START time (first_seq). Scalar
    // `loop_iteration` is no longer monotonic post-Option-3 — the
    // runtime's currentLoopIteration returns max() across containing
    // loops so an outer-loop counter can dominate every attempt of an
    // inner loop, scrambling the pip order if we sorted on it.
    for (const list of m.values()) {
      list.sort((a, b) => a.first_seq - b.first_seq);
    }
    return m;
  }, [executions]);

  // Fan-out regions + per-node replication, derived from branch_ids.
  const fanout = useMemo(() => computeFanout(executions), [executions]);
  // Keep the .then() refs in sync with the latest derived/incoming
  // values.
  useEffect(() => {
    execsByNodeRef.current = execsByNode;
    iterationByNodeRef.current = iterationByNode;
    runtimeOverrideByNodeRef.current = runtimeOverrideByNode;
    selectedNodeIdRef.current = selectedNodeId;
  }, [execsByNode, iterationByNode, runtimeOverrideByNode, selectedNodeId]);

  const handleSelectIteration = useCallback(
    (nodeId: string, iteration: number) => {
      onSelectIteration(nodeId, iteration);
      // Also select the node so the detail panel follows the picked
      // iteration without an extra click.
      onSelectNode(nodeId);
    },
    [onSelectIteration, onSelectNode],
  );

  // Index WireWorkflow nodes for the patch effect's meta refresh —
  // avoids re-walking wf.nodes on every patch.
  const wireNodeById = useMemo(() => {
    const m = new Map<string, WireNode>();
    if (wf) for (const n of wf.nodes) m.set(n.id, n);
    return m;
  }, [wf]);

  // Layout pass — runs once when the IR arrives. Iteration changes
  // and execution flips are handled by the patch effect below.
  useEffect(() => {
    if (!wf) return;
    let cancelled = false;
    const baseNodes: FlowNode[] = wf.nodes.map((n) => {
      const execs = execsByNode.get(n.id) ?? [];
      const selectedIteration =
        iterationByNode.get(n.id) ?? defaultIterationFor(execs);
      const meta = buildLLMMeta(
        n,
        runtimeOverrideByNode.get(n.id),
        effortCapsByPair,
      );
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
          meta,
          replication: fanout.replicationByNode.get(n.id) ?? null,
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
        labelBgStyle: { fill: "var(--color-surface-0)", opacity: 0.9 },
        labelBgPadding: [4, 2],
        style:
          isLoop
            ? { strokeDasharray: "8 4", stroke: loopStroke }
            : conditional
            ? { strokeDasharray: "4 3" }
            : undefined,
      };
    });

    autoLayout(baseNodes, baseEdges, layoutDirection)
      .then((laid) => {
        if (cancelled) return;
        // Re-derive data from REFS pointing at the current state —
        // not the snapshot captured when this effect fired. ELK
        // layout is ~50–200ms; in that window node_started events,
        // selection changes, and effortCapsByPair fetches can all
        // arrive. Without these refs the layout's setNodes commits
        // stale `executions: []` / `selected: false` on top of the
        // patch effect's already-applied update.
        const finalNodes = laid.map((fn) => {
          const wireNode = wireNodeById.get(fn.id);
          const execs = execsByNodeRef.current.get(fn.id) ?? [];
          const selectedIteration =
            iterationByNodeRef.current.get(fn.id) ?? defaultIterationFor(execs);
          const meta = wireNode
            ? buildLLMMeta(
                wireNode,
                runtimeOverrideByNodeRef.current.get(fn.id),
                effortCapsByPairRef.current,
              )
            : undefined;
          return {
            ...fn,
            data: {
              ...(fn.data as Record<string, unknown>),
              executions: execs,
              selectedIteration,
              selected: fn.id === selectedNodeIdRef.current,
              meta,
              replication: fanout.replicationByNode.get(fn.id) ?? null,
            },
          };
        });
        // Frame the fan-out replicated region(s) behind the real nodes so
        // it reads as "everything in here runs once per item".
        const frames = buildFanoutFrames(
          laid,
          fanout.regionNodesByRouter,
          fanout.routerItems,
        );
        setNodes([...frames, ...finalNodes]);
        setEdges(baseEdges);
        // Viewport positioning is owned by the effects below — the
        // initial-focus effect frames the running node(s) on entry
        // (same UX as the focus-running button), and the
        // selectedNodeId/layoutEpoch effect handles jump-to-failed +
        // live-tracking. A rAF fitView here would fire AFTER those
        // effects and clobber their framing.
        setLayoutEpoch((v) => v + 1);
      })
      .catch(() => {
        if (cancelled) return;
        setNodes(baseNodes);
        setEdges(baseEdges);
      });
    return () => {
      cancelled = true;
    };
    // Layout runs on `wf` change and on `layoutDirection` toggle —
    // both warrant a full ELK relayout. Iteration/execution flips,
    // selection, dimming, and async-arriving effort defaults all flow
    // through the patch effect below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wf, layoutDirection]);

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
        const wireNode = wireNodeById.get(n.id);
        const meta = wireNode
          ? buildLLMMeta(
              wireNode,
              runtimeOverrideByNode.get(n.id),
              effortCapsByPair,
            )
          : undefined;
        return {
          ...n,
          data: {
            ...(n.data as Record<string, unknown>),
            executions: execs,
            selectedIteration,
            selected: n.id === selectedNodeId,
            onSelectIteration: handleSelectIteration,
            meta,
            replication: fanout.replicationByNode.get(n.id) ?? null,
          },
          style: dimmed ? { opacity: 0.25 } : undefined,
        };
      }),
    );
  }, [
    wf,
    execsByNode,
    fanout,
    iterationByNode,
    selectedNodeId,
    handleSelectIteration,
    activeFilters,
    wireNodeById,
    runtimeOverrideByNode,
    effortCapsByPair,
  ]);

  // Centre on the selected node when selection changes + on layout
  // settle (handles the IR-fetch race). Pulses for ~600ms.
  useSelectedNodeFocus({ selectedNodeId, layoutEpoch, nodes, setNodes });

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

  // Distinct IR node ids that currently have at least one running
  // execution. Drives RunCanvasToolbar's "focus running" action: a
  // single click reframes the viewport onto these nodes. Empty when
  // the run is finished/paused/failed — the toolbar disables the
  // button in that case.
  const runningNodeIds = useMemo(() => {
    const set = new Set<string>();
    for (const [nodeId, execs] of execsByNode) {
      if (execs.some((ex) => ex.status === "running")) set.add(nodeId);
    }
    return set;
  }, [execsByNode]);

  // Initial focus on arrival: once layout has settled AND a running
  // node is known, frame the viewport on the running node(s).
  useInitialRunningFocus({ runId, layoutEpoch, nodes, runningNodeIds });

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

  const filterChips = buildFilterChips(filterCounts);

  return (
    <div className="h-full w-full relative">
      {wf.stale_hash && (
        <div className="absolute top-2 left-1/2 -translate-x-1/2 z-[var(--z-canvas)] px-2 py-1 text-caption rounded bg-warning-soft text-warning-fg border border-warning/60 shadow">
          ⚠ The .bot source has changed since this run started; the
          structure shown may differ from what executed.
        </div>
      )}
      <StatusLegend />

      <div className="absolute top-2 right-2 z-[var(--z-canvas)] flex items-center gap-1">
        <FilterChips
          chips={filterChips}
          activeFilters={activeFilters}
          onToggle={toggleFilter}
        />
        <RunCanvasToolbar
          onFitView={() =>
            reactFlow.fitView({ padding: 0.2, duration: 300 })
          }
          onFocusRunning={() => {
            if (runningNodeIds.size === 0) return;
            reactFlow.fitView({
              nodes: Array.from(runningNodeIds).map((id) => ({ id })),
              padding: 0.3,
              duration: 350,
              minZoom: 0.5,
              maxZoom: 1.5,
            });
          }}
          runningCount={runningNodeIds.size}
          onToggleLayoutDirection={toggleLayoutDirection}
          followLive={followLive}
          onToggleFollowLive={onToggleFollowLive}
        />
      </div>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        colorMode={resolvedTheme}
        fitView
        fitViewOptions={{ padding: 0.2 }}
        minZoom={0.05}
        maxZoom={4}
        onNodeClick={(_e, n) => onSelectNode(n.id === selectedNodeId ? null : n.id)}
        onPaneClick={() => onSelectNode(null)}
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={16} size={1} />
        <Controls showInteractive={true} />
      </ReactFlow>
    </div>
  );
}
