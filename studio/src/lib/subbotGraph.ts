import type { Node, Edge as FlowEdge } from "@xyflow/react";
import type { IterDocument, SubbotDecl } from "@/api/types";
import { documentToGraph, getTopologyKey } from "./documentToGraph";
import type { NodeData } from "./documentToGraph";
import { NODE_COLORS } from "./constants";

// Pure expansion of subbot nodes into inline child-workflow frames on the
// EDITOR canvas. A `subbot` declaration runs another .bot file as a child
// run; this module replaces the compact subbot node with a container frame
// (type "subbotFrame") holding the child bot's whole graph, and rewires the
// parent's edges so the flow reads end-to-end:
//   dispatch -> [frame: produce -> review -> wrap -> done] -> collect
//
// One level only: subbots nested inside a child document are NOT expanded —
// they render as compact subbot nodes inside the frame (documentToGraph
// already includes them in the node map). Expanding recursively would need
// cycle detection and per-level document fetching for marginal value.

/** Separator between a subbot node id and a child node id. DSL identifiers
 *  cannot contain ":" so `id.includes("::")` is a reliable external marker. */
export const SUBBOT_CHILD_SEP = "::";

export function makeSubbotChildId(subbotId: string, childId: string): string {
  return `${subbotId}${SUBBOT_CHILD_SEP}${childId}`;
}

/** True for nodes that belong to an expanded subbot's child graph. These are
 *  display-only: not editable, not connectable, not drill-in targets. */
export function isSubbotChildId(id: string): boolean {
  return id.includes(SUBBOT_CHILD_SEP);
}

/** Splits a child node id back into { subbotId, childId }; null for other ids. */
export function parseSubbotChildId(id: string): { subbotId: string; childId: string } | null {
  const idx = id.indexOf(SUBBOT_CHILD_SEP);
  if (idx <= 0) return null;
  return { subbotId: id.slice(0, idx), childId: id.slice(idx + SUBBOT_CHILD_SEP.length) };
}

/** Resolved child document (or load failure) for one subbot declaration. */
export interface SubbotChildDoc {
  // Workspace-relative path the subbot's `source` resolved to.
  path: string;
  doc?: IterDocument;
  error?: string;
}

export interface SubbotFrameData extends Record<string, unknown> {
  label: string;
  kind: "subbot";
  color: string;
  decl: SubbotDecl;
  source: string;
  // Resolved workspace-relative path of the child file (for the open button).
  sourcePath: string;
  isolated: boolean;
  childWorkflowName: string;
}

// Initial frame size before ELK sizes the compound node (mirrors the
// expanded-group placeholder in applyGroups).
const FRAME_INITIAL_W = 420;
const FRAME_INITIAL_H = 320;

/** POSIX-join a subbot `source` against the directory of the parent file.
 *  Both inputs are workspace-relative; the result is normalized (no ".",
 *  ".." collapsed — never escaping above the workspace root). */
export function resolveSubbotSource(parentFilePath: string | null, source: string): string {
  const dir = parentFilePath ? parentFilePath.split("/").slice(0, -1).join("/") : "";
  const joined = dir ? `${dir}/${source}` : source;
  const parts: string[] = [];
  for (const seg of joined.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (parts.length > 0) parts.pop();
      continue;
    }
    parts.push(seg);
  }
  return parts.join("/");
}

/** Topology fingerprint of the expansion state: which subbots have a loaded
 *  (or failed) child document, and each loaded child's own topology. Feeds
 *  the editor's relayout key so ELK re-runs when a child doc arrives. */
export function getSubbotExpansionKey(
  doc: IterDocument | null,
  childDocs: Map<string, SubbotChildDoc>,
): string {
  if (!doc || !doc.subbots || doc.subbots.length === 0) return "";
  const parts: string[] = [];
  for (const sb of doc.subbots) {
    const child = childDocs.get(sb.name);
    if (!child) {
      parts.push(`${sb.name}:pending`);
    } else if (child.error !== undefined || !child.doc) {
      parts.push(`${sb.name}:error`);
    } else {
      const wfName = child.doc.workflows?.[0]?.name;
      parts.push(`${sb.name}:${child.path}:${getTopologyKey(child.doc, wfName)}`);
    }
  }
  return parts.join(";;");
}

/** Expands every subbot node whose child document is loaded into a container
 *  frame holding the child graph. Pure: returns new node/edge arrays.
 *
 *  - Child ids are prefixed `${subbotId}::`; children get parentId +
 *    extent:"parent" (React Flow compound) and data.external = true.
 *  - The child's virtual __start__ node and its entry edge are dropped.
 *  - Parent edges INTO the subbot retarget to the child's entry node;
 *    parent edges OUT re-source from the child's `done` node when present
 *    (else stay on the frame). Edge labels/conditions are preserved.
 *  - A subbot whose child failed to load stays compact with data.loadError.
 */
export function expandSubbots(
  base: { nodes: Node<NodeData>[]; edges: FlowEdge[] },
  doc: IterDocument,
  childDocs: Map<string, SubbotChildDoc>,
): { nodes: Node[]; edges: FlowEdge[] } {
  const subbots = doc.subbots ?? [];
  if (subbots.length === 0) return base;

  const declByName = new Map(subbots.map((sb) => [sb.name, sb]));

  let nodes: Node[] = [...base.nodes];
  let edges: FlowEdge[] = [...base.edges];

  for (const [name, decl] of declByName) {
    const child = childDocs.get(name);
    const compactIdx = nodes.findIndex((n) => n.id === name);
    if (compactIdx === -1) continue; // subbot not on this workflow's canvas

    if (!child || !child.doc) {
      if (child?.error !== undefined) {
        // Load failure: keep the compact node, annotate it for the badge.
        const compact = nodes[compactIdx]!;
        nodes[compactIdx] = {
          ...compact,
          data: { ...compact.data, loadError: child.error, sourcePath: child.path },
        };
      }
      continue; // pending or unresolvable — compact node stays
    }

    const childDoc = child.doc;
    const childWorkflow = childDoc.workflows?.[0];
    const childGraph = documentToGraph(childDoc, childWorkflow?.name);
    const entryId = childWorkflow?.entry;

    const childNodes = childGraph.nodes
      .filter((n) => n.id !== "__start__")
      .map((n) => ({
        ...n,
        id: makeSubbotChildId(name, n.id),
        parentId: name,
        extent: "parent" as const,
        ariaLabel: `${n.data.kind} node: ${n.data.label} (part of subbot ${name})`,
        data: {
          ...n.data,
          external: true,
          subbotId: name,
          subbotSource: decl.source ?? "",
        },
      }));

    const childEdges = childGraph.edges
      .filter((e) => e.source !== "__start__")
      .map((e) => ({
        ...e,
        id: makeSubbotChildId(name, e.id),
        source: makeSubbotChildId(name, e.source),
        target: makeSubbotChildId(name, e.target),
      }));

    const hasEntry = entryId != null && childNodes.some((n) => n.id === makeSubbotChildId(name, entryId));
    const hasDone = childNodes.some((n) => n.id === makeSubbotChildId(name, "done"));

    const compact = nodes[compactIdx]!;
    const frame: Node<SubbotFrameData> = {
      id: name,
      type: "subbotFrame",
      position: compact.position,
      style: { width: FRAME_INITIAL_W, height: FRAME_INITIAL_H },
      ariaLabel: `subbot ${name}: child workflow from ${decl.source ?? ""}`,
      data: {
        label: name,
        kind: "subbot",
        color: NODE_COLORS.subbot,
        decl,
        source: decl.source ?? "",
        sourcePath: child.path,
        isolated: decl.isolated ?? false,
        childWorkflowName: childWorkflow?.name ?? "",
      },
    };

    // Frame must precede its children in the array (React Flow requires
    // parents before children); splice both in at the compact node's slot.
    nodes = [...nodes.slice(0, compactIdx), frame, ...childNodes, ...nodes.slice(compactIdx + 1)];

    // Retarget and re-source independently (no early return): a
    // self-loop edge on the subbot (`sub -> sub as retry(n)`) needs
    // BOTH rewires — done back around to the entry.
    edges = edges.map((e) => {
      let next = e;
      if (e.target === name && hasEntry) {
        next = { ...next, target: makeSubbotChildId(name, entryId!) };
      }
      if (e.source === name && hasDone) {
        next = { ...next, source: makeSubbotChildId(name, "done") };
      }
      return next;
    });
    edges = [...edges, ...childEdges];
  }

  return { nodes, edges };
}
