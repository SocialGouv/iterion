import type { Node } from "@xyflow/react";
import type { IterDocument, WorkflowDecl } from "@/api/types";

// Static detection of fan_out_each replicated regions for the EDITOR canvas.
// Unlike the run canvas (which derives the region from branch_ids in the run
// executions), the editor has no runtime — the region is read off the static
// graph: a fan_out_each router's single outgoing edge, traced forward until
// the convergence node (a node with `await: wait_all`) or a terminal. Every
// node on that path runs once PER ITEM; the router, the join, and everything
// up/downstream run once. Mirrors the engine's branch boundary.

const FRAME_PAD = 26;
const FRAME_HEADER = 16;
// Real rendered footprint of an editor workflowNode (wider/taller than the
// ELK layout size) — so the frame doesn't clip wide nodes. Mirrors the run
// canvas's FANOUT_NODE_W/H lesson.
const NODE_W = 210;
const NODE_H = 130;

export interface FanoutRegion {
  router: string;
  nodeIds: Set<string>;
}

// computeFanoutRegions returns one region per fan_out_each router in the
// active workflow: the set of node ids that run once per item.
export function computeFanoutRegions(
  doc: IterDocument | null,
  wf: WorkflowDecl | undefined | null,
): FanoutRegion[] {
  if (!doc || !wf) return [];
  const fanRouters = (doc.routers ?? []).filter((r) => r.mode === "fan_out_each");
  if (fanRouters.length === 0) return [];

  // Node → await mode (wait_all marks the convergence boundary). Cover ALL
  // node kinds: a real bot's join can be a tool, agent, judge, compute, or
  // human — omitting any kind would miss its wait_all boundary and let the
  // region bleed past the join.
  const awaitOf = new Map<string, string | undefined>();
  for (const a of doc.agents ?? []) awaitOf.set(a.name, a.await);
  for (const j of doc.judges ?? []) awaitOf.set(j.name, j.await);
  for (const t of doc.tools ?? []) awaitOf.set(t.name, t.await);
  for (const h of doc.humans ?? []) awaitOf.set(h.name, h.await);
  for (const c of doc.computes ?? []) awaitOf.set(c.name, c.await);

  // Forward adjacency from the active workflow's edges.
  const out = new Map<string, string[]>();
  for (const e of wf.edges ?? []) {
    let lst = out.get(e.from);
    if (!lst) out.set(e.from, (lst = []));
    lst.push(e.to);
  }

  const terminals = new Set(["done", "fail"]);
  const regions: FanoutRegion[] = [];
  for (const r of fanRouters) {
    const region = new Set<string>();
    const stack = [...(out.get(r.name) ?? [])];
    while (stack.length) {
      const n = stack.pop()!;
      if (region.has(n)) continue;
      // Boundary: the wait_all join (or a terminal) ends the per-item region
      // and is NOT part of it.
      if (awaitOf.get(n) === "wait_all" || terminals.has(n)) continue;
      region.add(n);
      for (const nxt of out.get(n) ?? []) stack.push(nxt);
    }
    if (region.size > 0) regions.push({ router: r.name, nodeIds: region });
  }
  // Self-diagnostic: a fan_out_each router that yields no region is almost
  // always an edge-trace miss (the active workflow's edges don't start at the
  // router) or a boundary collapse — surface it in dev instead of silently
  // drawing nothing.
  if (fanRouters.length > 0 && regions.length === 0 && import.meta.env?.DEV) {
    console.warn(
      "[fanout] fan_out_each router(s) present but no replicated region detected —",
      "check the active workflow's edges / wait_all boundary:",
      fanRouters.map((r) => ({ router: r.name, outgoing: out.get(r.name) ?? [] })),
    );
  }
  return regions;
}

// buildFanoutFrames produces one synthetic background frame node per region,
// sized to the bounding box of its laid-out nodes. Drawn behind the real
// nodes (zIndex −1, non-interactive). Nodes inside an expanded group (with a
// parentId) use parent-relative positions, so the frame is best-effort there;
// the common (ungrouped) case is exact.
export function buildFanoutFrames(layoutNodes: Node[], regions: FanoutRegion[]): Node[] {
  if (regions.length === 0) return [];
  const pos = new Map(layoutNodes.map((n) => [n.id, n.position]));
  const frames: Node[] = [];
  for (const reg of regions) {
    let minX = Infinity,
      minY = Infinity,
      maxX = -Infinity,
      maxY = -Infinity;
    for (const id of reg.nodeIds) {
      const p = pos.get(id);
      if (!p) continue;
      minX = Math.min(minX, p.x);
      minY = Math.min(minY, p.y);
      maxX = Math.max(maxX, p.x + NODE_W);
      maxY = Math.max(maxY, p.y + NODE_H);
    }
    if (!isFinite(minX)) continue;
    const w = maxX - minX + FRAME_PAD * 2;
    const h = maxY - minY + FRAME_PAD * 2 + FRAME_HEADER;
    frames.push({
      id: `__fanout__${reg.router}`,
      type: "fanoutFrame",
      position: { x: minX - FRAME_PAD, y: minY - FRAME_PAD - FRAME_HEADER },
      // Pre-set dimensions + `measured` so React Flow treats this synthetic
      // node as already measured. The EDITOR canvas is a CONTROLLED flow whose
      // onNodesChange only updates nodes present in `layoutNodes`; this frame
      // is injected in `displayNodes`, so React Flow's measurement change for
      // it is dropped — and without a committed `measured`, React Flow keeps
      // the node `visibility: hidden` forever (the run canvas avoids this only
      // because it has no onNodesChange). Providing the size up front makes the
      // frame visible without that round-trip.
      width: w,
      height: h,
      measured: { width: w, height: h },
      draggable: false,
      selectable: false,
      zIndex: -1,
      data: { width: w, height: h, label: `fan_out_each · ${reg.router}` },
    } as Node);
  }
  return frames;
}
