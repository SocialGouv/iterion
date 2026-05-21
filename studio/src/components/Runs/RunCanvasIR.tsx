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

import type {
  ExecStatus,
  ExecutionState,
  WireNode,
  WireWorkflow,
} from "@/api/runs";
import { getRunWorkflow } from "@/api/runs";
import { autoLayout } from "@/lib/autoLayout";
import type { DelegateOutputMeta } from "@/lib/delegateMeta";
import {
  effortBackendKey,
  useEffortCapabilitiesClient,
} from "@/hooks/useEffortCapabilities";
import type { EffortCapabilities } from "@/api/client";

import { useUIStore } from "@/store/ui";

import IRNode, { iterationColor, type LLMMeta } from "./IRNode";
import { statusClasses, type UnifiedStatus } from "./runStatusClasses";
import RunCanvasToolbar from "./RunCanvasToolbar";

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

function buildLLMMeta(
  node: WireNode,
  override: DelegateOutputMeta | undefined,
  effortCapsByPair: Map<string, EffortCapabilities>,
): LLMMeta | undefined {
  const declared = {
    model: node.model,
    backend: node.backend,
    effort: node.reasoning_effort,
  };
  if (!declared.model && !declared.backend && !declared.effort && !override) {
    return undefined;
  }
  const activeModel = override?.model ?? declared.model;
  // Caps lookup keys off the *declared* model because that's what
  // the prefetch effect populates the cache with. Runtime events
  // sometimes log a canonical alias (e.g. event "gpt-5.5" vs .iter
  // "openai/gpt-5.5") — the registry returns identical caps for
  // both, but the cache is keyed by the literal string.
  const capsModel = declared.model ?? activeModel;
  const caps = capsModel
    ? effortCapsByPair.get(
        `${effortBackendKey(declared.backend)} ${capsModel}`,
      )
    : undefined;
  // Effective effort priority:
  //   1. runtime override (event llm_request)
  //   2. value declared in the .iter (post-expansion would only show up
  //      via the override path, so a literal here is what the user wrote)
  //   3. provider's documented default from the registry — flagged so
  //      the badge renders attenuated.
  let activeEffort = override?.reasoning_effort ?? declared.effort;
  let effortIsResolvedDefault = false;
  if (!activeEffort && caps?.default) {
    activeEffort = caps.default;
    effortIsResolvedDefault = true;
  }
  return {
    model: activeModel,
    backend: declared.backend,
    reasoningEffort: activeEffort,
    runtimeOverriddenModel:
      !!override?.model && !!declared.model && override.model !== declared.model,
    runtimeOverriddenEffort:
      !!override?.reasoning_effort &&
      !!declared.effort &&
      override.reasoning_effort !== declared.effort,
    effortIsResolvedDefault,
    effortSupported: caps?.supported ?? undefined,
    contextWindow: override?.contextWindow,
    contextUsed: override?.contextUsed,
  };
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

// Compute the "current" iteration INDEX for an IR node — i.e. the
// 0-based position in the (start-ordered) executions array we want to
// land on when the user first opens the run console. Priority is the
// in-flight execution first, then a paused one, then the latest
// (last-started) one. Returns 0 when there are no executions yet.
//
// Index semantics — NOT scalar `loop_iteration` — because Option 3
// nested-loop exec_ids can produce multiple executions of the same node
// sharing the same scalar `loop_iteration` (e.g., the runtime's
// `currentLoopIteration` returns max() across containing loops and an
// outer loop counter can stay stuck dominating the inner counter for
// many iterations). Indexing into the start-ordered array is the only
// stable per-execution identifier the UI can key on.
export function defaultIterationFor(execs: ExecutionState[]): number {
  if (execs.length === 0) return 0;
  let runningIdx: number | undefined;
  let pausedIdx: number | undefined;
  for (let i = 0; i < execs.length; i++) {
    const e = execs[i]!;
    if (e.status === "running" && runningIdx === undefined) runningIdx = i;
    if (e.status === "paused_waiting_human" && pausedIdx === undefined) pausedIdx = i;
  }
  if (runningIdx !== undefined) return runningIdx;
  if (pausedIdx !== undefined) return pausedIdx;
  return execs.length - 1;
}

type StatusFilter = "running" | "paused" | "failed";

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
  const [wf, setWf] = useState<WireWorkflow | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [nodes, setNodes] = useState<FlowNode[]>([]);
  const [edges, setEdges] = useState<FlowEdge[]>([]);
  // Bumped each time ELK layout settles so the centering effect below
  // can re-fire once `nodes` actually exists — the existing
  // [selectedNodeId] dep alone misses the entry race where selectedNodeId
  // is set before the IR fetch + autoLayout complete (nodes=[] → silent
  // exit, and the next dep change is too late).
  const [layoutEpoch, setLayoutEpoch] = useState(0);
  const [activeFilters, setActiveFilters] = useState<Set<StatusFilter>>(
    () => new Set(),
  );
  // Effort capabilities (supported levels + default) keyed by
  // `${backend} ${model}`. Populated once the workflow lands by
  // walking unique pairs and asking /api/effort-capabilities.
  // buildLLMMeta uses `default` to render an attenuated badge when
  // the .iter declares no effort, and `supported` to normalise the
  // bar fill so a model's max always renders fully.
  const [effortCapsByPair, setEffortCapsByPair] = useState<
    Map<string, EffortCapabilities>
  >(() => new Map());
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

  const effortClient = useEffortCapabilitiesClient();

  // Prefetch effort capabilities for each unique (backend, model)
  // pair on the IR. Shares the React Query cache populated by AgentForm
  // so the editor side panel and the run canvas don't double-fetch.
  // Already-cached pairs are seeded synchronously so the bar normalises
  // and the attenuated badge render on first paint; the rest update as
  // fetches resolve.
  useEffect(() => {
    if (!wf) return;
    let cancelled = false;
    // Compute pairs OUTSIDE the state updater. React StrictMode
    // invokes the updater twice; mutating shared state (a Set) inside
    // the updater would make the second invocation skip everything
    // and commit an empty Map.
    const seen = new Set<string>();
    const seedEntries: Array<[string, EffortCapabilities]> = [];
    const toFetch: Array<{ key: string; backend: string; model: string }> = [];
    for (const n of wf.nodes) {
      if (!n.model) continue;
      const backend = effortBackendKey(n.backend);
      const key = `${backend} ${n.model}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const cached = effortClient.getCached(backend, n.model);
      if (cached) seedEntries.push([key, cached]);
      else toFetch.push({ key, backend, model: n.model });
    }
    if (seedEntries.length > 0) {
      setEffortCapsByPair((prev) => {
        let mutated = false;
        const next = new Map(prev);
        for (const [key, caps] of seedEntries) {
          if (next.get(key) !== caps) {
            next.set(key, caps);
            mutated = true;
          }
        }
        return mutated ? next : prev;
      });
    }
    for (const { key, backend, model } of toFetch) {
      // Capability lookup is best-effort; on failure the canvas
      // simply renders no badge for unset effort.
      effortClient
        .fetch(backend, model)
        .then((caps) => {
          if (cancelled) return;
          setEffortCapsByPair((prev) => {
            if (prev.get(key) === caps) return prev;
            const next = new Map(prev);
            next.set(key, caps);
            return next;
          });
        })
        .catch(() => {});
    }
    return () => {
      cancelled = true;
    };
  }, [wf, effortClient]);

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
            },
          };
        });
        setNodes(finalNodes);
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

  // Index WireWorkflow nodes for the patch effect's meta refresh —
  // avoids re-walking wf.nodes on every patch.
  const wireNodeById = useMemo(() => {
    const m = new Map<string, WireNode>();
    if (wf) for (const n of wf.nodes) m.set(n.id, n);
    return m;
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
    wireNodeById,
    runtimeOverrideByNode,
    effortCapsByPair,
  ]);

  // Centre on the selected node when selection changes (jump-to-failed,
  // running node advances) AND when the layout itself settles (initial
  // mount: parent's snapshot can populate selectedNodeId BEFORE the IR
  // fetch + ELK layout produce `nodes`, so depending on selectedNodeId
  // alone leaves us silently exited with nodes=[]). `layoutEpoch` bumps
  // exactly once per autoLayout completion, so per-event nodes patches
  // (executions advancing, iteration changes) don't re-fire setCenter.
  useEffect(() => {
    if (!selectedNodeId) return;
    const node = nodes.find((n) => n.id === selectedNodeId);
    if (!node) return;
    reactFlow.setCenter(
      node.position.x + 100,
      node.position.y + 40,
      { zoom: 1, duration: 350 },
    );
    // Pulse the freshly-selected node for ~600ms so the user sees the
    // jump even when the canvas is already showing the target — a
    // common case after clicking an EventLog row whose node is the
    // current viewport's centre. The pulse is purely additive
    // (transient class on the FlowNode wrapper) and clears itself so
    // ref-counted highlight state stays absent from the React tree.
    setNodes((prev) =>
      prev.map((n) =>
        n.id === selectedNodeId
          ? { ...n, className: `${n.className ?? ""} pulse-flash` }
          : n,
      ),
    );
    const t = setTimeout(() => {
      setNodes((prev) =>
        prev.map((n) =>
          n.id === selectedNodeId
            ? {
                ...n,
                className: (n.className ?? "").replace(" pulse-flash", "").trim(),
              }
            : n,
        ),
      );
    }, 600);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedNodeId, layoutEpoch]);

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
  // node is known, frame the viewport on the running node(s) — the
  // exact same call the "Center on running node" toolbar button makes.
  // Marks done so subsequent running-node changes (next node starts
  // during the run) don't keep re-zooming under the user. Resets on
  // runId change so navigating to a different run focuses again.
  const initialFocusDoneRef = useRef(false);
  useEffect(() => {
    initialFocusDoneRef.current = false;
  }, [runId]);
  useEffect(() => {
    if (initialFocusDoneRef.current) return;
    if (nodes.length === 0) return;
    if (runningNodeIds.size === 0) return;
    initialFocusDoneRef.current = true;
    const targets = Array.from(runningNodeIds).map((id) => ({ id }));
    // No cleanup: cancelling this rAF would defeat the whole point —
    // the patch effect's setNodes re-fires our deps within the same
    // frame, and a cleanup-based cancelAnimationFrame would clobber
    // the rAF before it runs. The `done` flag prevents re-scheduling.
    requestAnimationFrame(() => {
      reactFlow.fitView({
        nodes: targets,
        padding: 0.3,
        duration: 350,
        minZoom: 0.5,
        maxZoom: 1.5,
      });
    });
  }, [layoutEpoch, runningNodeIds, nodes, reactFlow]);

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
      <StatusLegend />

      <div className="absolute top-2 right-2 z-[var(--z-canvas)] flex items-center gap-1">
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

// StatusLegend explains the canvas node colour palette. Collapsed by
// default to keep the run viewport uncluttered — the "?" toggle in the
// bottom-left expands a small card showing each status with its colour
// chip and the matching label, so first-time viewers can map "red
// border" → "failed" without having to dig through docs.
function StatusLegend() {
  const [open, setOpen] = useState(false);
  const entries: Array<{ key: UnifiedStatus; sample: string }> = [
    { key: "running", sample: "bg-info-soft border-info" },
    { key: "finished", sample: "bg-success-soft border-success/60" },
    { key: "failed", sample: "bg-danger-soft border-danger/60" },
    { key: "paused_waiting_human", sample: "bg-warning-soft border-warning/60" },
    { key: "skipped", sample: "bg-surface-2 border-border-default" },
    { key: "none", sample: "bg-surface-1 border-border-default" },
  ];
  return (
    <div className="absolute bottom-2 left-2 z-30">
      {open ? (
        <div className="bg-surface-1/95 backdrop-blur border border-border-default rounded shadow-lg p-2 min-w-[180px]">
          <div className="flex items-center justify-between gap-2 mb-1">
            <span className="text-[10px] font-semibold text-fg-default">
              Node colours
            </span>
            <button
              type="button"
              className="text-[10px] text-fg-subtle hover:text-fg-default"
              onClick={() => setOpen(false)}
            >
              ×
            </button>
          </div>
          <ul className="space-y-0.5">
            {entries.map((e) => {
              const meta = statusClasses(e.key);
              return (
                <li key={e.key} className="flex items-center gap-2 text-[10px]">
                  <span
                    aria-hidden
                    className={`inline-block h-2.5 w-3.5 rounded border ${e.sample}`}
                  />
                  <span className="text-fg-default">{meta.label}</span>
                </li>
              );
            })}
          </ul>
        </div>
      ) : (
        <button
          type="button"
          onClick={() => setOpen(true)}
          className="bg-surface-1/90 backdrop-blur border border-border-default rounded h-6 w-6 text-fg-subtle hover:text-fg-default text-xs"
          title="Show node-colour legend"
        >
          ?
        </button>
      )}
    </div>
  );
}
