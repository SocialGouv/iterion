import type { Node, Edge as FlowEdge } from "@xyflow/react";
import { isAuxiliaryNodeId } from "./documentToGraph";
import { NODE_WIDTH, NODE_HEIGHT, AUX_NODE_WIDTH, AUX_NODE_HEIGHT } from "./constants";

type Side = "top" | "right" | "bottom" | "left";

interface Point {
  x: number;
  y: number;
}

interface Rect {
  x: number;
  y: number;
  width: number;
  height: number;
}

function getNodeDimensions(node: Node): { width: number; height: number } {
  if (node.measured?.width && node.measured?.height) {
    return { width: node.measured.width, height: node.measured.height };
  }
  if (isAuxiliaryNodeId(node.id)) {
    return { width: AUX_NODE_WIDTH, height: AUX_NODE_HEIGHT };
  }
  return { width: NODE_WIDTH, height: NODE_HEIGHT };
}

/** Get the anchor point (center of a side) for a node */
function getHandleAnchor(pos: Point, width: number, height: number, side: Side): Point {
  switch (side) {
    case "top":
      return { x: pos.x + width / 2, y: pos.y };
    case "bottom":
      return { x: pos.x + width / 2, y: pos.y + height };
    case "left":
      return { x: pos.x, y: pos.y + height / 2 };
    case "right":
      return { x: pos.x + width, y: pos.y + height / 2 };
  }
}

/** Check if a line segment from p1 to p2 intersects a rectangle (with padding) */
function lineIntersectsRect(p1: Point, p2: Point, rect: Rect, padding: number = 20): boolean {
  const r = {
    x: rect.x - padding,
    y: rect.y - padding,
    width: rect.width + padding * 2,
    height: rect.height + padding * 2,
  };

  // Check if the segment intersects the expanded rectangle using parametric clipping (Cohen-Sutherland-like)
  let tMin = 0;
  let tMax = 1;
  const dx = p2.x - p1.x;
  const dy = p2.y - p1.y;

  // Check each edge of the rectangle
  const edges = [
    { p: -dx, q: p1.x - r.x },              // left
    { p: dx, q: r.x + r.width - p1.x },      // right
    { p: -dy, q: p1.y - r.y },               // top
    { p: dy, q: r.y + r.height - p1.y },      // bottom
  ];

  for (const { p, q } of edges) {
    if (Math.abs(p) < 1e-10) {
      // Parallel to this edge
      if (q < 0) return false;
    } else {
      const t = q / p;
      if (p < 0) {
        tMin = Math.max(tMin, t);
      } else {
        tMax = Math.min(tMax, t);
      }
      if (tMin > tMax) return false;
    }
  }

  return true;
}

/** Pick the best source and target sides based on relative position */
function pickPrimarySides(
  srcCenter: Point,
  tgtCenter: Point,
): { sourceSide: Side; targetSide: Side } {
  const dx = tgtCenter.x - srcCenter.x;
  const dy = tgtCenter.y - srcCenter.y;

  // Very close nodes: fall back to default vertical
  if (Math.abs(dx) < 10 && Math.abs(dy) < 10) {
    return { sourceSide: "bottom", targetSide: "top" };
  }

  if (Math.abs(dx) > Math.abs(dy)) {
    // Horizontal dominance
    if (dx > 0) {
      return { sourceSide: "right", targetSide: "left" };
    } else {
      return { sourceSide: "left", targetSide: "right" };
    }
  } else {
    // Vertical dominance
    if (dy > 0) {
      return { sourceSide: "bottom", targetSide: "top" };
    } else {
      return { sourceSide: "top", targetSide: "bottom" };
    }
  }
}

/** Generate ranked alternative side combinations */
function getAlternativeCombinations(
  srcCenter: Point,
  tgtCenter: Point,
  primary: { sourceSide: Side; targetSide: Side },
): { sourceSide: Side; targetSide: Side }[] {
  const dx = tgtCenter.x - srcCenter.x;
  const dy = tgtCenter.y - srcCenter.y;
  const alternatives: { sourceSide: Side; targetSide: Side }[] = [];

  if (Math.abs(dx) > Math.abs(dy)) {
    // Primary is horizontal; secondary is vertical
    const vertSource: Side = dy > 0 ? "bottom" : "top";
    const vertTarget: Side = dy > 0 ? "top" : "bottom";
    alternatives.push({ sourceSide: vertSource, targetSide: primary.targetSide });
    alternatives.push({ sourceSide: primary.sourceSide, targetSide: vertTarget });
    alternatives.push({ sourceSide: vertSource, targetSide: vertTarget });
  } else {
    // Primary is vertical; secondary is horizontal
    const horizSource: Side = dx > 0 ? "right" : "left";
    const horizTarget: Side = dx > 0 ? "left" : "right";
    alternatives.push({ sourceSide: horizSource, targetSide: primary.targetSide });
    alternatives.push({ sourceSide: primary.sourceSide, targetSide: horizTarget });
    alternatives.push({ sourceSide: horizSource, targetSide: horizTarget });
  }

  return alternatives;
}

/** Pick sides for loop edges (backward edges) that wrap around */
function pickLoopSides(
  srcCenter: Point,
  tgtCenter: Point,
  direction: "DOWN" | "RIGHT",
): { sourceSide: Side; targetSide: Side } {
  if (direction === "DOWN") {
    // In vertical layout, loops go upward — use lateral sides
    if (srcCenter.x <= tgtCenter.x) {
      return { sourceSide: "left", targetSide: "left" };
    } else {
      return { sourceSide: "right", targetSide: "right" };
    }
  } else {
    // In horizontal layout, loops go leftward — use vertical sides
    if (srcCenter.y <= tgtCenter.y) {
      return { sourceSide: "top", targetSide: "top" };
    } else {
      return { sourceSide: "bottom", targetSide: "bottom" };
    }
  }
}

/**
 * Compute optimal sourceHandle and targetHandle for each edge based on
 * node positions, minimizing edge-node overlaps.
 */
export function computeEdgeHandles(
  nodes: Node[],
  edges: FlowEdge[],
  direction: "DOWN" | "RIGHT",
): FlowEdge[] {
  // Build position + dimension lookup
  const nodeInfo = new Map<
    string,
    { pos: Point; width: number; height: number; center: Point }
  >();

  for (const node of nodes) {
    const { width, height } = getNodeDimensions(node);
    const pos = node.position;
    nodeInfo.set(node.id, {
      pos,
      width,
      height,
      center: { x: pos.x + width / 2, y: pos.y + height / 2 },
    });
  }

  // Build node rects for intersection testing (excluding source and target of current edge)
  const allRects: { id: string; rect: Rect }[] = [];
  for (const [id, info] of nodeInfo) {
    allRects.push({
      id,
      rect: { x: info.pos.x, y: info.pos.y, width: info.width, height: info.height },
    });
  }

  return edges.map((edge) => {
    const srcInfo = nodeInfo.get(edge.source);
    const tgtInfo = nodeInfo.get(edge.target);

    if (!srcInfo || !tgtInfo) {
      // Node not found (shouldn't happen), leave edge as-is
      return edge;
    }

    const isLoop = !!(edge.data as Record<string, unknown>)?.loop;

    let sourceSide: Side = "bottom";
    let targetSide: Side = "top";

    if (isLoop) {
      ({ sourceSide, targetSide } = pickLoopSides(srcInfo.center, tgtInfo.center, direction));
    } else {
      const primary = pickPrimarySides(srcInfo.center, tgtInfo.center);

      // Check if primary path intersects any other node
      const srcAnchor = getHandleAnchor(srcInfo.pos, srcInfo.width, srcInfo.height, primary.sourceSide);
      const tgtAnchor = getHandleAnchor(tgtInfo.pos, tgtInfo.width, tgtInfo.height, primary.targetSide);

      const otherRects = allRects.filter((r) => r.id !== edge.source && r.id !== edge.target);
      const hasIntersection = otherRects.some((r) => lineIntersectsRect(srcAnchor, tgtAnchor, r.rect));

      if (!hasIntersection) {
        sourceSide = primary.sourceSide;
        targetSide = primary.targetSide;
      } else {
        // Try alternatives
        const alternatives = getAlternativeCombinations(srcInfo.center, tgtInfo.center, primary);
        let found = false;

        for (const alt of alternatives) {
          const altSrc = getHandleAnchor(srcInfo.pos, srcInfo.width, srcInfo.height, alt.sourceSide);
          const altTgt = getHandleAnchor(tgtInfo.pos, tgtInfo.width, tgtInfo.height, alt.targetSide);
          const altIntersects = otherRects.some((r) => lineIntersectsRect(altSrc, altTgt, r.rect));
          if (!altIntersects) {
            sourceSide = alt.sourceSide;
            targetSide = alt.targetSide;
            found = true;
            break;
          }
        }

        if (!found) {
          // All combinations intersect — use primary (least bad)
          sourceSide = primary.sourceSide;
          targetSide = primary.targetSide;
        }
      }
    }

    return {
      ...edge,
      sourceHandle: `source-${sourceSide}`,
      targetHandle: `target-${targetSide}`,
    };
  });
}
