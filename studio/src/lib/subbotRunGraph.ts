// Pure helpers for INLINE subbot expansion on the RUN canvas: once a
// subbot node has spawned child runs, the parent run's canvas replaces
// the compact subbot card with a frame holding the child workflow's own
// nodes (the pipeline that constitutes the subbot), each carrying the
// LIVE execution state of every child run — one pip per child, like a
// fan-out's multi-instance display. The editor-side twin (static, from
// the child .bot document) is lib/subbotGraph.ts; this one works on the
// WireWorkflow / ExecutionState shapes the run console lives on.
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
  // Subbot node id — doubles as the frame node id.
  id: string;
  source: string;
  isolated: boolean;
  childWorkflowName: string;
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

// expandWireSubbots expands every subbot node present in childWfByNode.
// Pure; returns the input wf untouched (frames: []) when nothing
// expands.
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

  for (const n of wf.nodes) {
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
    });

    const childIds = new Set(childWf.nodes.map((cn) => cn.id));
    for (const cn of childWf.nodes) {
      nodes.push({
        ...cn,
        id: makeSubbotChildId(n.id, cn.id),
        parentSubbot: n.id,
      });
    }

    // Rewire the parent edges end-to-end. Retarget and re-source
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

// Pip-order spacing: child i's sequence numbers are lifted into their
// own band so pips sort child-by-child (per-run seqs all start near 0
// and would interleave meaninglessly otherwise).
const CHILD_SEQ_BAND = 1_000_000;

// mergeChildExecutions projects each child run's execution states onto
// the expanded node ids so the run canvas renders them like any other
// execution:
//   - ir_node_id   -> `${subbotNodeId}::${childNodeId}` (frame-local)
//   - execution_id -> `${childRunId}::${execId}` (unique across children;
//                     childRunIdOfExecution reads it back for pip clicks)
//   - branch_id    -> `subrun::${childRunId}` (opts OUT of the fan-out
//                     region regex `^branch_...` — child-internal branch
//                     ids must not leak into the PARENT's fanout frames)
// Only nodes in expandedNodeIds contribute (a child run whose subbot
// node is not expanded keeps its executions off the parent canvas).
export function mergeChildExecutions(
  subRunsByNode: Map<string, RunSummary[]>,
  childExecutionsByRun: Map<string, ExecutionState[]>,
  expandedNodeIds: Set<string>,
): ExecutionState[] {
  const out: ExecutionState[] = [];
  for (const [nodeId, children] of subRunsByNode) {
    if (!expandedNodeIds.has(nodeId)) continue;
    children.forEach((child, ci) => {
      const execs = childExecutionsByRun.get(child.id);
      if (!execs) return;
      for (const ex of execs) {
        out.push({
          ...ex,
          ir_node_id: makeSubbotChildId(nodeId, ex.ir_node_id),
          execution_id: `${child.id}${SUBBOT_CHILD_SEP}${ex.execution_id}`,
          branch_id: `subrun${SUBBOT_CHILD_SEP}${child.id}`,
          first_seq: ci * CHILD_SEQ_BAND + ex.first_seq,
          last_seq: ci * CHILD_SEQ_BAND + ex.last_seq,
        });
      }
    });
  }
  return out;
}

// childRunIdOfExecution recovers the child run id a merged execution
// came from (see mergeChildExecutions' execution_id scheme). Null for
// the parent run's own executions.
export function childRunIdOfExecution(executionId: string): string | null {
  const idx = executionId.indexOf(SUBBOT_CHILD_SEP);
  if (idx <= 0) return null;
  return executionId.slice(0, idx);
}
