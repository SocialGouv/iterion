// Pure helpers for INLINE subbot expansion on the RUN canvas: once a
// subbot node has spawned child runs, the parent run's canvas replaces
// the compact subbot card with a frame holding the child workflow's own
// nodes (the pipeline that constitutes the subbot). The frame header
// carries one TAB per child run; the nodes inside show the live
// execution state of the SELECTED child only — switching tabs swaps
// the frame's content without ever leaving the parent run's canvas.
// The editor-side twin (static, from the child .bot document) is
// lib/subbotGraph.ts; this one works on the WireWorkflow /
// ExecutionState shapes the run console lives on.
//
// Expansion is per-node and data-driven: a subbot node expands only
// when its child workflow is known (fetched from the first child run's
// /workflow — all children of one subbot node share the source). Before
// any child exists the compact card (source + isolated + chip) stays.

import type { ExecutionState, RunSummary, WireWorkflow } from "@/api/runs";
import type { WireNode } from "@/api/runs";

import { SUBBOT_CHILD_SEP, makeSubbotChildId } from "./subbotGraph";

// A WireNode that belongs to an expanded subbot's child workflow.
// parentSubbot names the frame (= subbot node id) it renders inside.
export type ExpandedWireNode = WireNode & { parentSubbot?: string };

export interface InlineSubbotFrame {
  // Expanded subbot node id — doubles as the frame node id. Nested
  // frames chain the separator: `stage::step`.
  id: string;
  source: string;
  isolated: boolean;
  childWorkflowName: string;
  // Enclosing frame id for a NESTED frame (a subbot inside a subbot);
  // undefined for a top-level frame.
  parentSubbot?: string;
}

// WireWorkflow-shaped view: expanded subbot nodes are REPLACED by
// their child workflow's nodes (ids prefixed `${subbotId}::`), edges
// rewired end-to-end (into the child entry, out of the child done).
export interface ExpandedWireWorkflow extends Omit<WireWorkflow, "nodes"> {
  nodes: ExpandedWireNode[];
}

export interface ExpandedWire {
  wf: ExpandedWireWorkflow;
  frames: InlineSubbotFrame[];
}

// expandWireSubbots expands every subbot node present in childWfByNode
// — RECURSIVELY: childWfByNode is keyed by EXPANDED node id (a nested
// subbot's key is its chained id, e.g. "stage::step"), so a worklist
// pass expands frames within frames until no queued node matches.
// Pure; returns the input wf untouched (frames: []) when nothing
// expands. Termination: every expansion consumes one distinct map key
// (expanded ids are unique per position in the tree).
export function expandWireSubbots(
  wf: WireWorkflow,
  childWfByNode: Map<string, WireWorkflow>,
): ExpandedWire {
  const expandable = wf.nodes.filter(
    (n) => n.kind === "subbot" && childWfByNode.has(n.id),
  );
  if (expandable.length === 0) {
    return { wf, frames: [] };
  }

  const frames: InlineSubbotFrame[] = [];
  const nodes: ExpandedWireNode[] = [];
  let edges = [...wf.edges];

  const queue: ExpandedWireNode[] = [...wf.nodes];
  while (queue.length > 0) {
    const n = queue.shift()!;
    const childWf = n.kind === "subbot" ? childWfByNode.get(n.id) : undefined;
    if (!childWf) {
      nodes.push(n);
      continue;
    }

    frames.push({
      id: n.id,
      source: n.source ?? "",
      isolated: n.isolated ?? false,
      childWorkflowName: childWf.name,
      parentSubbot: n.parentSubbot,
    });

    const childIds = new Set(childWf.nodes.map((cn) => cn.id));
    // Queue (not push) the prefixed children so a nested subbot among
    // them can itself expand on a later worklist iteration.
    for (const cn of childWf.nodes) {
      queue.push({
        ...cn,
        id: makeSubbotChildId(n.id, cn.id),
        parentSubbot: n.id,
      });
    }

    // Rewire the enclosing edges end-to-end. Retarget and re-source
    // independently (no early return — a self-loop needs both).
    const hasEntry = childIds.has(childWf.entry);
    const hasDone = childIds.has("done");
    edges = edges.map((e) => {
      let next = e;
      if (e.to === n.id && hasEntry) {
        next = { ...next, to: makeSubbotChildId(n.id, childWf.entry) };
      }
      if (e.from === n.id && hasDone) {
        next = { ...next, from: makeSubbotChildId(n.id, "done") };
      }
      return next;
    });
    for (const ce of childWf.edges) {
      edges.push({
        ...ce,
        from: makeSubbotChildId(n.id, ce.from),
        to: makeSubbotChildId(n.id, ce.to),
      });
    }
  }

  return { wf: { ...wf, nodes, edges }, frames };
}

// mergeChildExecutions projects the SELECTED child run's execution
// states onto each frame's expanded node ids so the run canvas renders
// them like any other execution:
//   - ir_node_id   -> `${subbotNodeId}::${childNodeId}` (frame-local)
//   - execution_id -> `${childRunId}::${execId}` (unique when two
//                     frames' children share node names)
//   - branch_id    -> `subrun::${childRunId}` (opts OUT of the fan-out
//                     region regex `^branch_...` — child-internal branch
//                     ids must not leak into the PARENT's fanout frames)
// One child per frame: selectedChildByNode names the child run whose
// pipeline state the frame currently displays (the active tab).
export function mergeChildExecutions(
  subRunsByNode: Map<string, RunSummary[]>,
  childExecutionsByRun: Map<string, ExecutionState[]>,
  selectedChildByNode: Map<string, string>,
): ExecutionState[] {
  const out: ExecutionState[] = [];
  for (const [nodeId, children] of subRunsByNode) {
    const selectedId = selectedChildByNode.get(nodeId);
    if (!selectedId) continue;
    const child = children.find((c) => c.id === selectedId);
    if (!child) continue;
    const execs = childExecutionsByRun.get(child.id);
    if (!execs) continue;
    for (const ex of execs) {
      out.push({
        ...ex,
        ir_node_id: makeSubbotChildId(nodeId, ex.ir_node_id),
        execution_id: `${child.id}${SUBBOT_CHILD_SEP}${ex.execution_id}`,
        branch_id: `subrun${SUBBOT_CHILD_SEP}${child.id}`,
      });
    }
  }
  return out;
}
